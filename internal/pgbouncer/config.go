/*
 Copyright 2021 Crunchy Data Solutions, Inc.
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package pgbouncer

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

const (
	configDirectory = "/etc/pgbouncer"

	authFileAbsolutePath = configDirectory + "/" + authFileProjectionPath
	iniFileAbsolutePath  = configDirectory + "/" + iniFileProjectionPath

	authFileProjectionPath = "~postgres-operator/users.txt"
	iniFileProjectionPath  = "~postgres-operator.ini"

	authFileSecretKey   = "pgbouncer-users.txt" // #nosec G101 this is a name, not a credential
	credentialSecretKey = "pgbouncer-verifier"  // #nosec G101 this is a name, not a credential
	iniFileConfigMapKey = "pgbouncer.ini"
)

const (
	iniGeneratedWarning = "" +
		"# Generated by postgres-operator. DO NOT EDIT.\n" +
		"# Your changes will not be saved.\n"
)

type iniValueSet map[string]string

func (vs iniValueSet) String() string {
	keys := make([]string, 0, len(vs))
	for k := range vs {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		_, _ = fmt.Fprintf(&b, "%s = %s\n", k, vs[k])
	}
	return b.String()
}

// authFileContents returns a PgBouncer user database.
func authFileContents(password []byte) []byte {
	// > There should be at least 2 fields, surrounded by double quotes.
	// > Double quotes in a field value can be escaped by writing two double quotes.
	// - https://www.pgbouncer.org/config.html#authentication-file-format
	quote := func(s string) string {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}

	user1 := quote(postgresqlUser) + " " + quote(string(password)) + "\n"

	return []byte(user1)
}

func clusterINI(cluster *v1beta1.PostgresCluster) string {
	var (
		pgBouncerPort = *cluster.Spec.Proxy.PGBouncer.Port
		postgresPort  = *cluster.Spec.Port
	)

	// For versions of PgBouncer before v1.15, the global "auth_user" setting
	// must be placed before the first "[databases]" section.
	// - https://github.com/pgbouncer/pgbouncer/issues/391
	early := iniValueSet{"auth_user": postgresqlUser}

	// Use a wildcard to automatically create connection pools based on database
	// names. These pools connect to cluster's primary service. The service name
	// is an RFC 1123 DNS label so it does not need to be quoted nor escaped.
	// - https://www.pgbouncer.org/config.html#section-databases
	//
	// NOTE(cbandy): PgBouncer only accepts connections to items in this section
	// and the database "pgbouncer", which is the admin console. For connections
	// to the wildcard, PgBouncer first checks for the database in PostgreSQL.
	// When that database does not exist, the client will experience timeouts
	// or errors that sound like PgBouncer misconfiguration.
	// - https://github.com/pgbouncer/pgbouncer/issues/352
	// TODO(cbandy): allow the wildcard to be disabled.
	databases := fmt.Sprintf("[databases]\n* = host=%s port=%d\n",
		naming.ClusterPrimaryService(cluster).Name, postgresPort)

	defaults := iniValueSet{
		// Prior to PostgreSQL v12, the default setting for "extra_float_digits"
		// does not return precise float values. Applications that want
		// consistent results from different PostgreSQL versions may connect
		// with this startup parameter. The JDBC driver uses it regardless.
		// Trust that applications that know or care about this setting are
		// using it consistently within each connection pool.
		// - https://www.postgresql.org/docs/current/runtime-config-client.html#GUC-EXTRA-FLOAT-DIGITS
		// - https://github.com/pgjdbc/pgjdbc/blob/REL42.2.19/pgjdbc/src/main/java/org/postgresql/core/v3/ConnectionFactoryImpl.java#L334
		"ignore_startup_parameters": "extra_float_digits",
	}

	mandatory := iniValueSet{
		// Authenticate frontend connections using passwords stored in PostgreSQL.
		// PgBouncer will connect to the backend database that is requested by
		// the frontend as the "auth_user" and execute "auth_query". When
		// "auth_user" requires a password, PgBouncer reads it from "auth_file".
		"auth_file":  authFileAbsolutePath,
		"auth_query": "SELECT username, password from pgbouncer.get_auth($1)",
		"auth_user":  postgresqlUser,

		// TODO(cbandy): Use an HBA file to control authentication of PgBouncer
		// accounts; e.g. "admin_users" below.
		// - https://www.pgbouncer.org/config.html#hba-file-format
		//"auth_hba_file": "",
		//"auth_type":     "hba",
		//"admin_users": "pgbouncer",

		// Require TLS encryption on client connections.
		"client_tls_sslmode":   "require",
		"client_tls_cert_file": certFrontendAbsolutePath,
		"client_tls_key_file":  certFrontendPrivateKeyAbsolutePath,
		"client_tls_ca_file":   certFrontendAuthorityAbsolutePath,

		// Prevent the user from bypassing the main configuration file.
		"conffile": iniFileAbsolutePath,

		// Listen on the PgBouncer port on all addresses.
		"listen_addr": "*",
		"listen_port": fmt.Sprint(pgBouncerPort),

		// Require TLS encryption on connections to PostgreSQL.
		"server_tls_sslmode": "verify-full",
		"server_tls_ca_file": certBackendAuthorityAbsolutePath,

		// Disable Unix sockets to keep the filesystem read-only.
		"unix_socket_dir": "",
	}

	return iniGeneratedWarning +
		"\n[pgbouncer]\n" + early.String() + databases +
		"\n[pgbouncer]\n" + defaults.String() +
		"\n[pgbouncer]\n" + mandatory.String()
}

// podConfigFiles returns projections of PgBouncer's configuration files to
// include in the configuration volume.
func podConfigFiles(
	clusterConfigMap *corev1.ConfigMap, clusterSecret *corev1.Secret,
) []corev1.VolumeProjection {
	return []corev1.VolumeProjection{
		{
			ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: clusterConfigMap.Name,
				},
				Items: []corev1.KeyToPath{{
					Key:  iniFileConfigMapKey,
					Path: iniFileProjectionPath,
				}},
			},
		},
		{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: clusterSecret.Name,
				},
				Items: []corev1.KeyToPath{{
					Key:  authFileSecretKey,
					Path: authFileProjectionPath,
				}},
			},
		},
	}
}
