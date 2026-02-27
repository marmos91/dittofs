/*
Package percona provides utilities for building PerconaPGCluster specifications
from DittoServer configuration.
*/
package percona

import (
	"fmt"

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
// Returns an empty spec and nil error if Percona is not configured.
// Returns an error if the storage size is invalid.
func BuildPerconaPGClusterSpec(ds *dittoiov1alpha1.DittoServer) (pgv2.PerconaPGClusterSpec, error) {
	cfg := ds.Spec.Percona
	if cfg == nil {
		return pgv2.PerconaPGClusterSpec{}, nil
	}

	replicas := int32(1)
	if cfg.Replicas != nil {
		replicas = *cfg.Replicas
	}

	storageSize := DefaultStorageSize
	if cfg.StorageSize != "" {
		storageSize = cfg.StorageSize
	}

	// Parse storage size safely to avoid panic
	storageSizeQuantity, err := resource.ParseQuantity(storageSize)
	if err != nil {
		return pgv2.PerconaPGClusterSpec{}, fmt.Errorf("invalid percona.storageSize %q: %w", storageSize, err)
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
							corev1.ResourceStorage: storageSizeQuantity,
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

	// Configure backups: explicitly disable unless configured.
	// Percona defaults backups to enabled (IsEnabled returns true when Enabled is nil),
	// so we must set Enabled=false when no backup repos are configured.
	if cfg.Backup != nil && cfg.Backup.Enabled {
		spec.Backups = buildBackupsSpec(ds.Name, cfg.Backup)
	} else {
		disabled := false
		spec.Backups = pgv2.Backups{Enabled: &disabled}
	}

	return spec, nil
}

// Default backup schedules and region
const (
	defaultFullSchedule = "0 2 * * *" // Daily at 2am
	defaultIncrSchedule = "0 * * * *" // Hourly
	defaultBackupRegion = "eu-west-1"
)

// buildBackupsSpec creates the pgBackRest backup configuration.
func buildBackupsSpec(dsName string, backup *dittoiov1alpha1.PerconaBackupConfig) pgv2.Backups {
	if backup == nil || !backup.Enabled {
		return pgv2.Backups{}
	}

	fullSchedule := valueOrDefault(backup.FullSchedule, defaultFullSchedule)
	incrSchedule := valueOrDefault(backup.IncrSchedule, defaultIncrSchedule)
	region := valueOrDefault(backup.Region, defaultBackupRegion)

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

// valueOrDefault returns the value if non-empty, otherwise returns the default.
func valueOrDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}
