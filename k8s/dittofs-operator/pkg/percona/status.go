package percona

import (
	pgv2 "github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2"
)

// IsReady checks if the PerconaPGCluster is ready.
// A cluster is ready when its state is AppStateReady.
func IsReady(cluster *pgv2.PerconaPGCluster) bool {
	return cluster.Status.State == pgv2.AppStateReady
}

// GetState returns the current state of the PerconaPGCluster as a string.
func GetState(cluster *pgv2.PerconaPGCluster) string {
	return string(cluster.Status.State)
}
