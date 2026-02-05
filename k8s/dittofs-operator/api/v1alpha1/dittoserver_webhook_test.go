package v1alpha1

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
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

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsSubstring(s, substr)))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestStorageClassValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = AddToScheme(scheme)

	// Create a fake client with no StorageClass
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	validator := &DittoServerValidator{Client: fakeClient}

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
	scheme := runtime.NewScheme()
	_ = storagev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = AddToScheme(scheme)

	// Create a fake client with a StorageClass
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "existing-sc",
		},
		Provisioner: "kubernetes.io/aws-ebs",
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sc).Build()

	validator := &DittoServerValidator{Client: fakeClient}

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
	scheme := runtime.NewScheme()
	_ = storagev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = AddToScheme(scheme)

	// Create a fake client with no Secret
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	validator := &DittoServerValidator{Client: fakeClient}

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
	scheme := runtime.NewScheme()
	_ = storagev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = AddToScheme(scheme)

	// Create a fake client with a Secret that has missing keys
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
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	validator := &DittoServerValidator{Client: fakeClient}

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
	scheme := runtime.NewScheme()
	_ = storagev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = AddToScheme(scheme)

	// Create a fake client with a valid Secret
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
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	validator := &DittoServerValidator{Client: fakeClient}

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
