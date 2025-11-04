// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	ce "github.com/cloudevents/sdk-go/v2"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kubearchivev1 "github.com/kubearchive/kubearchive/cmd/operator/api/v1"
	kcel "github.com/kubearchive/kubearchive/pkg/cel"
	"github.com/kubearchive/kubearchive/pkg/cloudevents"
	"github.com/kubearchive/kubearchive/pkg/constants"
	"github.com/kubearchive/kubearchive/pkg/filters"
	"github.com/kubearchive/kubearchive/pkg/k8sclient"
)

type InformerInfo struct {
	GVR          schema.GroupVersionResource
	KindSelector kubearchivev1.APIVersionKind
	Namespaces   map[string]filters.CelExpressions
	Informer     cache.SharedIndexInformer
	StopCh       chan struct{}
	Queue        workqueue.RateLimitingInterface
	WorkerWg     sync.WaitGroup
}

type informerEventItem struct {
	eventType string
	obj       *unstructured.Unstructured
}

type SinkFilterReconciler struct {
	Client              client.Client
	Scheme              *runtime.Scheme
	Mapper              meta.RESTMapper
	dynamicClient       dynamic.Interface
	cloudEventPublisher *cloudevents.SinkCloudEventPublisher
	informerFactory     dynamicinformer.DynamicSharedInformerFactory

	// Mutex to protect informer operations
	mu sync.RWMutex
	// Map of GVK string to informer info
	informers map[string]*InformerInfo
}

//+kubebuilder:rbac:groups=kubearchive.org,resources=sinkfilters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kubearchive.org,resources=sinkfilters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kubearchive.org,resources=sinkfilters/finalizers,verbs=update
//+kubebuilder:rbac:groups=kubearchive.org,resources=kubearchiveconfigs;clusterkubearchiveconfigs,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *SinkFilterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	log.Info("Reconciling SinkFilter", "name", req.Name, "namespace", req.Namespace)

	sinkFilter := &kubearchivev1.SinkFilter{}
	err := r.Client.Get(ctx, req.NamespacedName, sinkFilter)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("SinkFilter resource not found. Ignoring since object must be deleted")
			// Clear all informers when the resource is deleted by calling generateInformers with empty maps.
			if err = r.generateInformers(ctx, map[string]map[string]filters.CelExpressions{}); err != nil {
				log.Error(err, "Failed to clear informers on delete")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get SinkFilter")
		return ctrl.Result{}, err
	}

	if err := r.reconcileClusterRole(ctx, sinkFilter); err != nil {
		log.Error(err, "Failed to reconcile ClusterRole")
		return ctrl.Result{}, err
	}

	if err := r.reconcileClusterRoleBinding(ctx); err != nil {
		log.Error(err, "Failed to reconcile ClusterRoleBinding")
		return ctrl.Result{}, err
	}

	namespacesByKinds := filters.ExtractAllNamespacesByKinds(sinkFilter)

	if err := r.generateInformers(ctx, namespacesByKinds); err != nil {
		log.Error(err, "Failed to generate informers")
		return ctrl.Result{}, err
	}

	log.Info("Successfully reconciled SinkFilter", "namespacesByKinds", len(namespacesByKinds))
	return ctrl.Result{}, nil
}

func (r *SinkFilterReconciler) parseKindAndAPIVersionFromKey(key string) (string, string) {
	// Key format is "Kind-APIVersion", so parse it back
	parts := strings.Split(key, "-")
	if len(parts) >= 2 {
		kind := parts[0]
		apiVersion := strings.Join(parts[1:], "-") // In case APIVersion contains dashes.
		return kind, apiVersion
	}
	return "", ""
}

func (r *SinkFilterReconciler) generateInformers(ctx context.Context, namespacesByKinds map[string]map[string]filters.CelExpressions) error {
	log := log.FromContext(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()

	toStop := r.findInformersToStop(namespacesByKinds)
	toCreate := r.findInformersToCreate(namespacesByKinds)
	toUpdate := r.findInformersToUpdate(namespacesByKinds, toStop)

	for key := range toStop {
		if informerInfo, exists := r.informers[key]; exists {
			close(informerInfo.StopCh)
			informerInfo.Queue.ShutDown()
			informerInfo.WorkerWg.Wait()
			delete(r.informers, key)
			log.Info("Stopped informer for resource", "key", key)
		}
	}

	for key := range toUpdate {
		if informerInfo, exists := r.informers[key]; exists {
			// Update the namespaces for this informer
			informerInfo.Namespaces = namespacesByKinds[key]
			log.Info("Updated informer namespaces", "key", key, "namespaceCount", len(informerInfo.Namespaces))
		}
	}

	for key := range toCreate {
		kind, apiVersion := r.parseKindAndAPIVersionFromKey(key)
		gvr, _, _, err := r.getGVRFromKindAndAPIVersion(kind, apiVersion)
		if err != nil {
			log.Error(err, "Failed to get GVR for kind", "kind", kind, "apiVersion", apiVersion)
			continue
		}

		r.createInformerForGVR(ctx, key, gvr, namespacesByKinds[key])
		log.Info("Created informer for resource", "gvr", gvr.String())
	}

	log.Info("Informer update complete",
		"stopped", len(toStop),
		"updated", len(toUpdate),
		"created", len(toCreate))

	return nil
}

func (r *SinkFilterReconciler) findInformersToStop(namespacesByKinds map[string]map[string]filters.CelExpressions) map[string]struct{} {
	toStop := make(map[string]struct{})
	for existingKey := range r.informers {
		if _, stillNeeded := namespacesByKinds[existingKey]; !stillNeeded {
			toStop[existingKey] = struct{}{}
		}
	}
	return toStop
}

func (r *SinkFilterReconciler) findInformersToCreate(namespacesByKinds map[string]map[string]filters.CelExpressions) map[string]struct{} {
	toCreate := make(map[string]struct{})
	for newKey := range namespacesByKinds {
		if _, exists := r.informers[newKey]; !exists {
			toCreate[newKey] = struct{}{}
		}
	}
	return toCreate
}

func (r *SinkFilterReconciler) findInformersToUpdate(namespacesByKinds map[string]map[string]filters.CelExpressions, toStop map[string]struct{}) map[string]struct{} {
	toUpdate := make(map[string]struct{})

	for existingKey := range r.informers {
		if _, stillNeeded := namespacesByKinds[existingKey]; stillNeeded {
			if _, stopping := toStop[existingKey]; !stopping {
				toUpdate[existingKey] = struct{}{}
			}
		}
	}

	return toUpdate
}

func (r *SinkFilterReconciler) getGVRFromKindAndAPIVersion(kind, apiVersion string) (schema.GroupVersionResource, string, string, error) {
	var gv schema.GroupVersion
	var err error

	if apiVersion == "" {
		return schema.GroupVersionResource{}, "", "", fmt.Errorf("APIVersion is required")
	}

	gv, err = schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, "", "", fmt.Errorf("failed to parse APIVersion %s: %w", apiVersion, err)
	}

	// Use the REST mapper to get the resource name
	gvk := schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    kind,
	}

	mapping, err := r.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, "", "", fmt.Errorf("failed to get REST mapping for %v: %w", gvk, err)
	}

	return mapping.Resource, kind, apiVersion, nil
}

func (r *SinkFilterReconciler) createInformerForGVR(ctx context.Context, key string, gvr schema.GroupVersionResource, namespaces map[string]filters.CelExpressions) {
	log := log.FromContext(ctx)
	stopCh := make(chan struct{})

	kind, apiVersion := r.parseKindAndAPIVersionFromKey(key)
	kindSelector := kubearchivev1.APIVersionKind{
		Kind:       kind,
		APIVersion: apiVersion,
	}

	queue := workqueue.NewRateLimitingQueueWithConfig(
		workqueue.DefaultControllerRateLimiter(),
		workqueue.RateLimitingQueueConfig{
			Name: key,
		},
	)

	informer := r.informerFactory.ForResource(gvr).Informer()

	informerInfo := &InformerInfo{
		GVR:          gvr,
		KindSelector: kindSelector,
		Namespaces:   namespaces,
		Informer:     informer,
		StopCh:       stopCh,
		Queue:        queue,
	}

	r.informers[key] = informerInfo

	_, err := informer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: r.filterFunc,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				r.handleInformerEventWrapper(ctx, informerInfo, "Added", obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				r.handleInformerEventWrapper(ctx, informerInfo, "Modified", newObj)
			},
			DeleteFunc: func(obj interface{}) {
				r.handleInformerEventWrapper(ctx, informerInfo, "Deleted", obj)
			},
		},
	})
	if err != nil {
		log.Error(err, "Failed to add event handler to informer", "key", key)
		return
	}

	numWorkers := 3
	for i := 0; i < numWorkers; i++ {
		informerInfo.WorkerWg.Add(1)
		go r.runWorker(ctx, informerInfo, key)
	}

	go informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		log.Error(nil, "Failed to sync cache for informer", "key", key)
		return
	}

	log.Info("Started informer for resource", "key", key, "gvr", gvr.String())
}

func (r *SinkFilterReconciler) handleInformerEventWrapper(ctx context.Context, informerInfo *InformerInfo, eventType string, obj interface{}) {
	log := log.FromContext(ctx)
	if !informerInfo.Informer.HasSynced() {
		return
	}
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Error(nil, "Unexpected object type in handler", "type", fmt.Sprintf("%T", obj), "eventType", eventType)
		return
	}
	informerInfo.Queue.Add(&informerEventItem{
		eventType: eventType,
		obj:       unstructuredObj,
	})
}

func (r *SinkFilterReconciler) filterFunc(obj interface{}) bool {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}

	kind := unstructuredObj.GetKind()
	apiVersion := unstructuredObj.GetAPIVersion()

	r.mu.RLock()
	defer r.mu.RUnlock()
	informerInfo, exists := r.informers[kind+"-"+apiVersion]
	if !exists {
		return false
	}

	namespace := unstructuredObj.GetNamespace()
	_, globalExists := informerInfo.Namespaces[constants.SinkFilterGlobalNamespace]
	_, namespaceExists := informerInfo.Namespaces[namespace]

	if !globalExists && !namespaceExists {
		return false
	}

	return true
}

func (r *SinkFilterReconciler) runWorker(ctx context.Context, informerInfo *InformerInfo, key string) {
	defer informerInfo.WorkerWg.Done()
	log := log.FromContext(ctx)

	for {
		select {
		case <-informerInfo.StopCh:
			log.Info("Stopping worker", "key", key)
			return
		default:
			item, shutdown := informerInfo.Queue.Get()
			if shutdown {
				return
			}

			func() {
				defer informerInfo.Queue.Done(item)

				eventItem, ok := item.(*informerEventItem)
				if !ok {
					log.Error(nil, "Unexpected item type in queue", "type", fmt.Sprintf("%T", item))
					return
				}

				if err := r.handleInformerEvent(ctx, eventItem.eventType, eventItem.obj, informerInfo); err != nil {
					log.Error(err, "Failed to handle informer event", "key", key, "eventType", eventItem.eventType)
					informerInfo.Queue.AddRateLimited(item)
					return
				}

				informerInfo.Queue.Forget(item)
			}()
		}
	}
}

func (r *SinkFilterReconciler) handleInformerEvent(ctx context.Context, eventType string, unstructuredObj *unstructured.Unstructured, informerInfo *InformerInfo) error {
	objNamespace := unstructuredObj.GetNamespace()
	globalCel, globalExists := informerInfo.Namespaces[constants.SinkFilterGlobalNamespace]
	namespaceCel, namespaceExists := informerInfo.Namespaces[objNamespace]

	if !globalExists && !namespaceExists {
		return nil
	}

	switch eventType {
	case "Added", "Modified":
		if (globalExists && kcel.ExecuteBooleanCEL(ctx, globalCel.DeleteWhen, unstructuredObj)) ||
			(namespaceExists && kcel.ExecuteBooleanCEL(ctx, namespaceCel.DeleteWhen, unstructuredObj)) {
			return r.sendCloudEvent(ctx, "delete-when", unstructuredObj, informerInfo)
		} else if (globalExists && kcel.ExecuteBooleanCEL(ctx, globalCel.ArchiveWhen, unstructuredObj)) ||
			(namespaceExists && kcel.ExecuteBooleanCEL(ctx, namespaceCel.ArchiveWhen, unstructuredObj)) {
			return r.sendCloudEvent(ctx, "archive-when", unstructuredObj, informerInfo)
		}
		return nil
	case "Deleted":
		if (globalExists && kcel.ExecuteBooleanCEL(ctx, globalCel.ArchiveOnDelete, unstructuredObj)) ||
			(namespaceExists && kcel.ExecuteBooleanCEL(ctx, namespaceCel.ArchiveOnDelete, unstructuredObj)) {
			return r.sendCloudEvent(ctx, "archive-on-delete", unstructuredObj, informerInfo)
		}
		return nil
	default:
		log.FromContext(ctx).Error(nil, "Ignoring unknown event type", "type", eventType)
		return nil
	}
}

func (r *SinkFilterReconciler) sendCloudEvent(ctx context.Context, eventType string, unstructuredObj *unstructured.Unstructured, informerInfo *InformerInfo) error {
	log := log.FromContext(ctx)

	uid := string(unstructuredObj.GetUID())
	name := unstructuredObj.GetName()
	namespace := unstructuredObj.GetNamespace()

	if r.cloudEventPublisher == nil {
		err := fmt.Errorf("CloudEvent publisher not available")
		log.Error(err, "Skipping event",
			"uid", uid,
			"name", name,
			"namespace", namespace,
			"eventType", eventType,
			"apiVersion", informerInfo.KindSelector.APIVersion,
			"kind", informerInfo.KindSelector.Kind)
		return err
	}

	resource := unstructuredObj.Object
	if resource["apiVersion"] == nil {
		if informerInfo.GVR.Group == "" {
			resource["apiVersion"] = informerInfo.GVR.Version
		} else {
			resource["apiVersion"] = informerInfo.GVR.Group + "/" + informerInfo.GVR.Version
		}
	}

	if resource["kind"] == nil && informerInfo.KindSelector.Kind != "" {
		resource["kind"] = informerInfo.KindSelector.Kind
	}

	var owner string
	ownerRefs := unstructuredObj.GetOwnerReferences()
	if len(ownerRefs) > 0 {
		owner = string(ownerRefs[0].UID)
	}

	result := r.cloudEventPublisher.Send(ctx, "org.kubearchive.sinkfilters.resource."+eventType, resource)
	if !ce.IsACK(result) {
		var err error
		if ce.IsNACK(result) {
			err = fmt.Errorf("cloud event was not acknowledged")
		} else {
			err = fmt.Errorf("cloud event send failed")
		}
		log.Error(err, "Failed to send cloud event",
			"uid", uid,
			"name", name,
			"namespace", namespace,
			"owner", owner,
			"eventType", eventType,
			"apiVersion", informerInfo.KindSelector.APIVersion,
			"kind", informerInfo.KindSelector.Kind,
			"result", result)
		return err
	}

	log.Info("Successfully sent cloud event",
		"uid", uid,
		"name", name,
		"namespace", namespace,
		"owner", owner,
		"eventType", eventType,
		"apiVersion", informerInfo.KindSelector.APIVersion,
		"kind", informerInfo.KindSelector.Kind)
	return nil
}

func (r *SinkFilterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var err error
	r.dynamicClient, err = k8sclient.NewInstrumentedDynamicClient()
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	r.cloudEventPublisher, err = cloudevents.NewSinkCloudEventPublisher("kubearchive.org/sinkfilter-controller")
	if err != nil {
		return fmt.Errorf("failed to create cloud event publisher: %w", err)
	}

	r.informerFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.dynamicClient, 10*time.Minute, metav1.NamespaceAll, nil)

	r.informers = make(map[string]*InformerInfo)

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubearchivev1.SinkFilter{}).
		Complete(r)
}

func (r *SinkFilterReconciler) extractResources(sinkFilter *kubearchivev1.SinkFilter) []kubearchivev1.APIVersionKind {
	resourcesMap := make(map[kubearchivev1.APIVersionKind]struct{})

	for _, namespaceResources := range sinkFilter.Spec.Namespaces {
		for _, resource := range namespaceResources {
			resourcesMap[resource.Selector] = struct{}{}
		}
	}

	resources := make([]kubearchivev1.APIVersionKind, 0, len(resourcesMap))
	for resource := range resourcesMap {
		resources = append(resources, resource)
	}

	return resources
}

func (r *SinkFilterReconciler) reconcileClusterRole(ctx context.Context, sinkFilter *kubearchivev1.SinkFilter) error {
	log := log.FromContext(ctx)

	resources := r.extractResources(sinkFilter)
	rules := createPolicyRules(ctx, r.Mapper, resources, []string{"get", "list", "watch"})

	desired := desiredClusterRole(constants.KubeArchiveSinkFilterName, rules)

	existing := &rbacv1.ClusterRole{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: constants.KubeArchiveSinkFilterName}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Creating ClusterRole", "name", constants.KubeArchiveSinkFilterName)
			return r.Client.Create(ctx, desired)
		}
		return fmt.Errorf("failed to get ClusterRole: %w", err)
	}

	if !equalPolicyRules(existing.Rules, rules) {
		log.Info("Updating ClusterRole", "name", constants.KubeArchiveSinkFilterName)
		existing.Rules = rules
		return r.Client.Update(ctx, existing)
	}

	return nil
}

func (r *SinkFilterReconciler) reconcileClusterRoleBinding(ctx context.Context) error {
	log := log.FromContext(ctx)

	subjects := []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      constants.KubeArchiveOperatorName,
			Namespace: constants.KubeArchiveNamespace,
		},
	}

	desired := desiredClusterRoleBinding(constants.KubeArchiveSinkFilterName, "ClusterRole", subjects...)

	existing := &rbacv1.ClusterRoleBinding{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: constants.KubeArchiveSinkFilterName}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Creating ClusterRoleBinding", "name", constants.KubeArchiveSinkFilterName)
			return r.Client.Create(ctx, desired)
		}
		return fmt.Errorf("failed to get ClusterRoleBinding: %w", err)
	}

	if !slices.Equal(existing.Subjects, desired.Subjects) ||
		existing.RoleRef != desired.RoleRef {
		log.Info("Updating ClusterRoleBinding", "name", constants.KubeArchiveSinkFilterName)
		existing.Subjects = desired.Subjects
		existing.RoleRef = desired.RoleRef
		return r.Client.Update(ctx, existing)
	}

	return nil
}

