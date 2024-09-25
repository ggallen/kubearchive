// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

func init() {
	RegisteredDatabases["postgresql"] = NewPostgreSQLDatabase
}

type PostgreSQLDatabaseInfo struct {
	*DatabaseInfo
}

func (info PostgreSQLDatabaseInfo) GetDriverName() string {
	return "postgres"
}

func (info PostgreSQLDatabaseInfo) GetConnectionString() string {
	return fmt.Sprintf("user=%s password=%s dbname=%s host=%s port=%s sslmode=disable", info.env[DbUserEnvVar],
		info.env[DbPasswordEnvVar], info.env[DbNameEnvVar], info.env[DbHostEnvVar], info.env[DbPortEnvVar])
}

func (info PostgreSQLDatabaseInfo) GetResourcesSQL() string {
	return "SELECT data FROM resource WHERE kind=$1 AND api_version=$2"
}

func (info PostgreSQLDatabaseInfo) GetNamespacedResourcesSQL() string {
	return "SELECT data FROM resource WHERE kind=$1 AND api_version=$2 AND namespace=$3"
}

func (info PostgreSQLDatabaseInfo) GetWriteResourceSQL() string {
	return "INSERT INTO resource (uuid, api_version, kind, name, namespace, resource_version, cluster_deleted_ts, data) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) " +
		"ON CONFLICT(uuid) DO UPDATE SET name=$4, namespace=$5, resource_version=$6, cluster_deleted_ts=$7, data=$8"
}

type PostgreSQLDatabase struct {
	*Database
}

func NewPostgreSQLDatabase() DBInterface {
	var db *sql.DB
	return PostgreSQLDatabase{&Database{db, PostgreSQLDatabaseInfo{&DatabaseInfo{}}}}
}
