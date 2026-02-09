/*
Package percona provides utilities for building PerconaPGCluster specifications
from DittoServer configuration.
*/
package percona

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	pgv2 "github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2"
	crunchyv1beta1 "github.com/percona/percona-postgresql-operator/v2/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

const (
	// PerconaAPIVersion is the API version for PerconaPGCluster
	PerconaAPIVersion = "2.8.0"
	// PostgresVersion is the PostgreSQL version to use
	PostgresVersion = 16
	// DefaultStorageSize is the default storage size for PostgreSQL
	DefaultStorageSize = "10Gi"
	// DefaultDatabaseName is the default database name
	DefaultDatabaseName = "dittofs"
	// DittoFSUser is the PostgreSQL user for DittoFS
	DittoFSUser = "dittofs"
)

// ClusterName returns the PerconaPGCluster name for a DittoServer
func ClusterName(dsName string) string {
	return dsName + "-postgres"
}

// SecretName returns the Percona user credentials Secret name.
// Percona creates Secrets named {cluster-name}-pguser-{user-name}.
func SecretName(dsName string) string {
	return ClusterName(dsName) + "-pguser-" + DittoFSUser
}

// BuildPerconaPGClusterSpec creates the PerconaPGCluster spec from DittoServer config.
// Returns an empty spec if Percona is not configured.
func BuildPerconaPGClusterSpec(ds *dittoiov1alpha1.DittoServer) pgv2.PerconaPGClusterSpec {
	cfg := ds.Spec.Percona
	if cfg == nil {
		return pgv2.PerconaPGClusterSpec{}
	}

	replicas := int32(1)
	if cfg.Replicas != nil {
		replicas = *cfg.Replicas
	}

	storageSize := DefaultStorageSize
	if cfg.StorageSize != "" {
		storageSize = cfg.StorageSize
	}

	dbName := DefaultDatabaseName
	if cfg.DatabaseName != "" {
		dbName = cfg.DatabaseName
	}

	spec := pgv2.PerconaPGClusterSpec{
		CRVersion:       PerconaAPIVersion,
		PostgresVersion: PostgresVersion,
		InstanceSets: []pgv2.PGInstanceSetSpec{
			{
				Name:     "instance1",
				Replicas: &replicas,
				DataVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(storageSize),
						},
					},
				},
			},
		},
		Users: []crunchyv1beta1.PostgresUserSpec{
			{
				Name:      crunchyv1beta1.PostgresIdentifier(DittoFSUser),
				Databases: []crunchyv1beta1.PostgresIdentifier{crunchyv1beta1.PostgresIdentifier(dbName)},
			},
		},
	}

	// Set storage class if specified
	if cfg.StorageClassName != nil {
		spec.InstanceSets[0].DataVolumeClaimSpec.StorageClassName = cfg.StorageClassName
	}

	// Configure backups if enabled
	if cfg.Backup != nil && cfg.Backup.Enabled {
		spec.Backups = buildBackupsSpec(ds.Name, cfg.Backup)
	}

	return spec
}

// buildBackupsSpec creates the pgBackRest backup configuration.
func buildBackupsSpec(dsName string, backup *dittoiov1alpha1.PerconaBackupConfig) pgv2.Backups {
	if backup == nil || !backup.Enabled {
		return pgv2.Backups{}
	}

	// Default schedules
	fullSchedule := "0 2 * * *" // Daily at 2am
	incrSchedule := "0 * * * *" // Hourly
	region := "eu-west-1"

	if backup.FullSchedule != "" {
		fullSchedule = backup.FullSchedule
	}
	if backup.IncrSchedule != "" {
		incrSchedule = backup.IncrSchedule
	}
	if backup.Region != "" {
		region = backup.Region
	}

	enabled := true
	backups := pgv2.Backups{
		Enabled: &enabled,
		PGBackRest: pgv2.PGBackRestArchive{
			Global: map[string]string{
				"repo1-path":               "/pgbackrest/" + dsName + "/repo1",
				"repo1-s3-uri-style":       "path",
				"repo1-storage-verify-tls": "y",
			},
			Repos: []crunchyv1beta1.PGBackRestRepo{
				{
					Name: "repo1",
					S3: &crunchyv1beta1.RepoS3{
						Bucket:   backup.Bucket,
						Endpoint: backup.Endpoint,
						Region:   region,
					},
					BackupSchedules: &crunchyv1beta1.PGBackRestBackupSchedules{
						Full:        &fullSchedule,
						Incremental: &incrSchedule,
					},
				},
			},
		},
	}

	// Add credentials secret reference if provided
	if backup.CredentialsSecretRef != nil {
		backups.PGBackRest.Configuration = []corev1.VolumeProjection{
			{
				Secret: &corev1.SecretProjection{
					LocalObjectReference: *backup.CredentialsSecretRef,
				},
			},
		}
	}

	return backups
}
