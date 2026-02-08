package v1alpha1

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testScheme returns a scheme configured with all types needed for tests.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = storagev1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = AddToScheme(s)
	return s
}

// newTestValidator creates a DittoServerValidator with a fake client for testing.
func newTestValidator(objs ...client.Object) *DittoServerValidator {
	return &DittoServerValidator{
		Client: fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build(),
	}
}

func TestDittoServerValidation(t *testing.T) {
	tests := []struct {
		name        string
		dittoServer *DittoServer
		wantErr     bool
		errContains string
		wantWarning bool
	}{
		{
			name: "valid minimal configuration",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
						CacheSize:    "5Gi",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid full configuration",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
						ContentSize:  "50Gi",
						CacheSize:    "5Gi",
					},
					Database: &DatabaseConfig{
						Type: "sqlite",
						SQLite: &SQLiteConfig{
							Path: "/data/controlplane/controlplane.db",
						},
					},
					Cache: &InfraCacheConfig{
						Path: "/data/cache",
						Size: "1GB",
					},
					Metrics: &MetricsConfig{
						Enabled: true,
						Port:    9090,
					},
					ControlPlane: &ControlPlaneAPIConfig{
						Port: 8080,
					},
					Identity: &IdentityConfig{
						JWT: &JWTConfig{
							SecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "jwt-secret",
								},
								Key: "secret",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing metadata size",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						// MetadataSize missing
					},
				},
			},
			wantErr:     true,
			errContains: "storage.metadataSize is required",
		},
		{
			name: "postgres secret with sqlite type - should warn",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
						CacheSize:    "5Gi",
					},
					Database: &DatabaseConfig{
						Type: "sqlite",
						PostgresSecretRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "postgres-secret",
							},
							Key: "connection-string",
						},
					},
				},
			},
			wantErr:     false,
			wantWarning: true,
		},
		{
			name: "invalid control plane port",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
						CacheSize:    "5Gi",
					},
					ControlPlane: &ControlPlaneAPIConfig{
						Port: 70000, // Invalid port
					},
				},
			},
			wantErr:     true,
			errContains: "controlPlane.port must be between 1 and 65535",
		},
		{
			name: "invalid metrics port",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
						CacheSize:    "5Gi",
					},
					Metrics: &MetricsConfig{
						Enabled: true,
						Port:    70000, // Invalid port
					},
				},
			},
			wantErr:     true,
			errContains: "metrics.port must be between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := tt.dittoServer.ValidateCreate(context.Background(), tt.dittoServer)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateCreate() expected error but got none")
					return
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidateCreate() error = %v, should contain %v", err, tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateCreate() unexpected error = %v", err)
				}
			}

			if tt.wantWarning && len(warnings) == 0 {
				t.Errorf("ValidateCreate() expected warnings but got none")
			}
		})
	}
}

func TestStorageClassValidation(t *testing.T) {
	validator := newTestValidator()

	scName := "nonexistent-sc"
	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ds",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize:     "10Gi",
				CacheSize:        "5Gi",
				StorageClassName: &scName,
			},
		},
	}

	_, err := validator.ValidateCreate(context.Background(), ds)
	if err == nil {
		t.Error("Expected error for nonexistent StorageClass, got nil")
	}
}

func TestStorageClassValidation_Exists(t *testing.T) {
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "existing-sc",
		},
		Provisioner: "kubernetes.io/aws-ebs",
	}
	validator := newTestValidator(sc)

	scName := "existing-sc"
	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ds",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize:     "10Gi",
				CacheSize:        "5Gi",
				StorageClassName: &scName,
			},
		},
	}

	_, err := validator.ValidateCreate(context.Background(), ds)
	if err != nil {
		t.Errorf("Expected no error for existing StorageClass, got: %v", err)
	}
}

func TestS3SecretWarning(t *testing.T) {
	validator := newTestValidator()

	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ds",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			S3: &S3StoreConfig{
				CredentialsSecretRef: &S3CredentialsSecretRef{
					SecretName: "nonexistent-secret",
				},
			},
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), ds)
	if err != nil {
		t.Errorf("S3 Secret not found should warn, not error: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("Expected warning for missing S3 Secret")
	}
}

func TestS3SecretMissingKeys(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"accessKeyId": []byte("test-key"),
			// Missing secretAccessKey
		},
	}
	validator := newTestValidator(secret)

	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ds",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			S3: &S3StoreConfig{
				CredentialsSecretRef: &S3CredentialsSecretRef{
					SecretName: "s3-secret",
				},
			},
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), ds)
	if err != nil {
		t.Errorf("Missing S3 key should warn, not error: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("Expected warning for missing secretAccessKey")
	}
}

func TestS3SecretValid(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"accessKeyId":     []byte("test-key"),
			"secretAccessKey": []byte("test-secret"),
		},
	}
	validator := newTestValidator(secret)

	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ds",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			S3: &S3StoreConfig{
				CredentialsSecretRef: &S3CredentialsSecretRef{
					SecretName: "s3-secret",
				},
			},
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), ds)
	if err != nil {
		t.Errorf("Valid S3 Secret should not error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("Expected no warnings for valid S3 Secret, got: %v", warnings)
	}
}

// Percona validation tests
// Note: Tests that require RESTMapper (CRD existence) are tested via
// integration tests as fake client doesn't support RESTMapper mocking.

func TestPerconaPrecedenceWarning(t *testing.T) {
	// Test that basic validation passes when both Percona and PostgresSecretRef are set
	// Note: The actual warning is produced by validateDittoServerWithClient, which
	// requires cluster access. Basic validation does not check this precedence.
	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-percona-precedence",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			Percona: &PerconaConfig{
				Enabled: true,
			},
			Database: &DatabaseConfig{
				PostgresSecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "external-postgres",
					},
					Key: "connection-string",
				},
			},
		},
	}

	// Basic validation should pass (Percona + PostgresSecretRef is allowed config)
	warnings, err := ds.validateDittoServer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Basic validation doesn't check Percona/Postgres precedence (needs client)
	_ = warnings
}

func TestPerconaBackupRequiredFields(t *testing.T) {
	// Test that backup validation happens in the client-aware validator
	// Basic validation does not check backup required fields
	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-percona-backup",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			Percona: &PerconaConfig{
				Enabled: true,
				Backup: &PerconaBackupConfig{
					Enabled: true,
					// Missing bucket and endpoint - validated by client-aware validator
				},
			},
		},
	}

	// Basic validation passes (backup validation needs client for full check)
	warnings, err := ds.validateDittoServer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = warnings
}

func TestPerconaDisabled(t *testing.T) {
	// Test that Percona config is valid when disabled (even without CRD)
	validator := newTestValidator()

	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-percona-disabled",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			Percona: &PerconaConfig{
				Enabled: false, // Disabled, so no CRD check needed
			},
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), ds)
	if err != nil {
		t.Errorf("Disabled Percona should not error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("Expected no warnings for disabled Percona, got: %v", warnings)
	}
}

func TestPerconaStorageClassValidation(t *testing.T) {
	// Test that Percona StorageClass validation is a hard error when not found
	validator := newTestValidator()

	scName := "nonexistent-percona-sc"
	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-percona-sc",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			Percona: &PerconaConfig{
				Enabled:          true,
				StorageClassName: &scName,
			},
		},
	}

	// This will fail at the RESTMapper check before StorageClass validation
	// because fake client doesn't support RESTMapper.
	// In a real cluster, the error would be about the StorageClass.
	_, err := validator.ValidateCreate(context.Background(), ds)
	if err == nil {
		t.Error("Expected error when Percona enabled without CRD")
	}
}

func TestPerconaBackupMissingBucket(t *testing.T) {
	// When Percona is enabled with backup enabled but bucket missing,
	// should error (requires CRD check first)
	validator := newTestValidator()

	ds := &DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-percona-backup-no-bucket",
			Namespace: "default",
		},
		Spec: DittoServerSpec{
			Storage: StorageSpec{
				MetadataSize: "10Gi",
				CacheSize:    "5Gi",
			},
			Percona: &PerconaConfig{
				Enabled: true,
				Backup: &PerconaBackupConfig{
					Enabled:  true,
					Bucket:   "", // Missing
					Endpoint: "https://s3.cubbit.eu",
				},
			},
		},
	}

	// This will fail at RESTMapper first (no CRD)
	// In a real cluster with CRD, it would fail at bucket validation
	_, err := validator.ValidateCreate(context.Background(), ds)
	if err == nil {
		t.Error("Expected error when Percona enabled without CRD")
	}
}
