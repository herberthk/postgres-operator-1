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

package pgmonitor

import (
	"context"
	"strings"

	"github.com/crunchydata/postgres-operator/internal/logging"
	"github.com/crunchydata/postgres-operator/internal/postgres"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

const (
	// MonitoringUser is a Postgres user created by pgMonitor configuration
	MonitoringUser = "ccp_monitoring"

	// TODO jmckulk: copied from pgbouncer; candidate for common package?
	// sqlCurrentAndFutureDatabases returns all the database names where pgMonitor
	// functions should be enabled or disabled. It includes the "template1"
	// database so that exporter is automatically enabled in future databases.
	// The "template0" database is explicitly excluded to ensure it is never
	// manipulated.
	// - https://www.postgresql.org/docs/current/managing-databases.html
	sqlCurrentAndFutureDatabases = "" +
		`SELECT datname FROM pg_catalog.pg_database` +
		` WHERE datallowconn AND datname NOT IN ('template0')`
)

// PostgreSQLHBAs provides the Postgres HBA rules for allowing the monitoring
// exporter to be accessible
func PostgreSQLHBAs(inCluster *v1beta1.PostgresCluster, outHBAs *postgres.HBAs) {
	if ExporterEnabled(inCluster) {
		// Kubernetes does guarantee localhost resolves to loopback:
		// https://kubernetes.io/docs/concepts/cluster-administration/networking/
		// https://releases.k8s.io/v1.21.0/pkg/kubelet/kubelet_pods.go#L343
		outHBAs.Mandatory = append(outHBAs.Mandatory, *postgres.NewHBA().TCP().
			User(MonitoringUser).Network("127.0.0.0/8").Method("md5"))
		outHBAs.Mandatory = append(outHBAs.Mandatory, *postgres.NewHBA().TCP().
			User(MonitoringUser).Network("::1/128").Method("md5"))
	}
}

// PostgreSQLParameters provides additional required configuration parameters
// that Postgres needs to support monitoring
func PostgreSQLParameters(inCluster *v1beta1.PostgresCluster, outParameters *postgres.Parameters) {
	if ExporterEnabled(inCluster) {
		// Exporter expects that shared_preload_libraries are installed
		// pg_stat_statements: https://access.crunchydata.com/documentation/pgmonitor/latest/exporter/
		// pgnodemx: https://github.com/CrunchyData/pgnodemx
		libraries := []string{"pg_stat_statements", "pgnodemx"}

		defined, found := outParameters.Mandatory.Get("shared_preload_libraries")
		if found {
			libraries = append(libraries, defined)
		}

		outParameters.Mandatory.Add("shared_preload_libraries", strings.Join(libraries, ","))
	}
}

// DisableExporterInPostgreSQL disables the exporter configuration in PostgreSQL.
// Currently the exporter is disabled by removing login permissions for the
// monitoring user.
// TODO: evaluate other uninstall/removal options
func DisableExporterInPostgreSQL(ctx context.Context, exec postgres.Executor) error {
	log := logging.FromContext(ctx)

	stdout, stderr, err := postgres.Executor(exec).ExecInDatabasesFromQuery(ctx,
		`SELECT pg_catalog.current_database();`,
		strings.TrimSpace(`
		SELECT pg_catalog.format('ALTER ROLE %I NOLOGIN', :'username')
		 WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'username')
		\gexec`),
		map[string]string{
			"username": MonitoringUser,
		})

	log.V(1).Info("monitoring user disabled", "stdout", stdout, "stderr", stderr)

	return err
}

// EnableExporterInPostgreSQL runs SQL setup commands in `database` to enable
// the exporter to retrieve metrics. pgMonitor objects are created and expected
// extensions are installed. We also ensure that the monitoring user has the
// current password and can login.
func EnableExporterInPostgreSQL(ctx context.Context, exec postgres.Executor,
	monitoringSecret *corev1.Secret, database, setup string) error {
	log := logging.FromContext(ctx)

	stdout, stderr, err := postgres.Executor(exec).ExecInDatabasesFromQuery(ctx,
		sqlCurrentAndFutureDatabases,
		strings.Join([]string{
			// Exporter expects that extension(s) to be installed in all databases
			// pg_stat_statements: https://access.crunchydata.com/documentation/pgmonitor/latest/exporter/
			"CREATE EXTENSION IF NOT EXISTS pg_stat_statements;",
		}, "\n"),
		nil,
	)

	log.V(1).Info("applied pgMonitor objects", "database", "current and future databases", "stdout", stdout, "stderr", stderr)

	// NOTE: Setup is run last to ensure that the setup sql is used in the hash
	if err == nil {
		stdout, stderr, err = postgres.Executor(exec).ExecInDatabasesFromQuery(ctx,
			database,
			strings.Join([]string{
				// Setup.sql file from the exporter image. sql is specific
				// to the PostgreSQL version
				setup,

				// pgnodemx: https://github.com/CrunchyData/pgnodemx
				// The `monitor` schema is hard-coded in the setup SQL files
				// from pgMonitor configuration
				// https://github.com/CrunchyData/pgmonitor/blob/master/exporter/postgres/queries_nodemx.yml
				"CREATE EXTENSION IF NOT EXISTS pgnodemx WITH SCHEMA monitor;",

				// ccp_monitoring user is created in Setup.sql without a
				// password; update the password and ensure that the ROLE
				// can login to the database
				`ALTER ROLE :"username" LOGIN PASSWORD :'verifier';`,
			}, "\n"),
			map[string]string{
				"username": MonitoringUser,
				"verifier": string(monitoringSecret.Data["verifier"]),
			},
		)

		log.V(1).Info("applied pgMonitor objects", "database", database, "stdout", stdout, "stderr", stderr)
	}

	return err
}
