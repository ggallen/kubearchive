# kubearchive

## Requirements
To deploy kubearchive locally you need to install the following:
* podman
* jq
* helm
* kubectl
* kind
* cosign

On fedora, install podman, jq, and helm with this command:
```bash
sudo dnf install podman jq helm
```
Otherwise follow the [podman](https://podman.io/docs/installation), [jq](https://jqlang.github.io/jq/download/), and [helm](https://helm.sh/docs/intro/install/) install instructions.

Follow the [kubernetes](https://kubernetes.io/docs/tasks/tools/#kubectl), [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation), and [cosign](https://docs.sigstore.dev/system_config/installation/) install instructions.

## Local Deployment
Create a cluster using kind.
```bash
kind create cluster
```
By default the cluster name is `kind`. You can choose a name by using the `--name` flag.
If you are still getting this error after following the instructions [here](https://kind.sigs.k8s.io/docs/user/rootless/)
```
ERROR: failed to create cluster: running kind with rootless provider requires setting systemd property "Delegate=yes", see https://kind.sigs.k8s.io/docs/user/rootless/
```
try creating the cluster with this command:
```bash
systemd-run -p Delegate=yes --user --scope kind create cluster
```

After the cluster is created, run the kubectl command printed by kind to set your kube context to the kind cluster.

Verfiy the image signatures for knative:
```bash
curl -sSL https://github.com/knative/serving/releases/download/knative-v1.13.1/serving-core.yaml \
  | grep 'gcr.io/' | awk '{print $2}' | sort | uniq \
  | xargs -n 1 \
    cosign verify -o text \
      --certificate-identity=signer@knative-releases.iam.gserviceaccount.com \
      --certificate-oidc-issuer=https://accounts.google.com
```

Finally run the helm chart to delploy kubearchive:
```bash
helm install [deployment name] charts/kubearchive
```
You can use the `-g` flag to have helm generate a deployment name for you.

Run this command remove the the kubearchive deployment:
```bash
helm uninstall [deployment name]
```

## Kubearchive Helm Chart

The kubearchive helm chart deploys the following:
* Namespace named `kubearchive`
* ClusterRole named `kubearchive`
* ClusterRoleBinding named `kubearchive`
* Service Account named `kubearchive` in the `kubearchive` namespace
* ApiServerSource named `api-server-source` in the `kubearchive` namespace
* Deployment and Service for `kubearchive-sink` in the `kubearchive` namespace
* (optionally) Namespace named `test`

### ApiServerSource Configuration
The ApiServerSource deployed by this helm chart uses the `kubearchive` service account to watch resources
on the cluster. By default it is deployed to watch for events. The `ClusterRole` and `ClusterRoleBinding`
by default give the kubearchive service account permissions to `get`, `list`, and `watch` `Events` cluster-wide.
The ApiServerSource is deploy by default to only listen for events in namepspaces with the label `kubearchive: watch`.
The `test` namespace, if created, has that label applied. The resources that the ApiServerSource listens for can be
changed by running the helm chart with `kubearchive.role.rules[0].resources` and `apiServerSource.resources` overridden.
`kubearchive.role.rules[0].resources` expects that that the resource type(s) list are all lowercase and plural. If one
or more of the resources in `kubearchive.role.rules[0].resources` is not in the kubernetes core API group, then
`kubearchive.role.rules[0].apiGroups` must be overridden as well to include all API groups that contain all the 
resources that you are interested in. `apiServerSource.resources` is a list where each item includes the `kind` and
`apiVersion` of the resouce.

### kubearchive-sink
An ApiServerSource requires a sink that it can send cloud events to. Right now, kubearchive-sink is
`https://github.com/knative-sample/event-display/` which is a simple sink written in go that prints
all cloud events it receives to `stdout`. You can view cloud events it receives with the following
command:
```bash
kubectl logs --namespace=kubearchive -l app=kubearchive-sink --tail=1000
```
**`event-display` is just a placeholder. It needs to be replaced with a sink that is written for kubearchive.**
