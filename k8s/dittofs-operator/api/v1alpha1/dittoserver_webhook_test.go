package v1alpha1

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
