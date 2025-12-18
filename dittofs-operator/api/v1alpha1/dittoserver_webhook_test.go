package v1alpha1

import (
	"context"
	"testing"

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
			name: "valid configuration",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "badger-meta", Type: "badger"},
							{Name: "local-content", Type: "local"},
						},
						Shares: []ShareConfig{
							{
								Name:          "test-share",
								ExportPath:    "/export",
								MetadataStore: "badger-meta",
								ContentStore:  "local-content",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing metadata store reference",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "local-content", Type: "local"},
						},
						Shares: []ShareConfig{
							{
								Name:          "test-share",
								ExportPath:    "/export",
								MetadataStore: "badger-meta", // Does not exist
								ContentStore:  "local-content",
							},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "metadataStore 'badger-meta' does not exist in backends list",
		},
		{
			name: "missing content store reference",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "badger-meta", Type: "badger"},
						},
						Shares: []ShareConfig{
							{
								Name:          "test-share",
								ExportPath:    "/export",
								MetadataStore: "badger-meta",
								ContentStore:  "missing-store", // Does not exist
							},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "contentStore 'missing-store' does not exist in backends list",
		},
		{
			name: "duplicate backend names",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "badger-meta", Type: "badger"},
							{Name: "badger-meta", Type: "badger"}, // Duplicate
						},
						Shares: []ShareConfig{
							{
								Name:          "test-share",
								ExportPath:    "/export",
								MetadataStore: "badger-meta",
								ContentStore:  "badger-meta",
							},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "duplicate backend name 'badger-meta'",
		},
		{
			name: "duplicate share names",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "badger-meta", Type: "badger"},
							{Name: "local-content", Type: "local"},
						},
						Shares: []ShareConfig{
							{
								Name:          "test-share",
								ExportPath:    "/export",
								MetadataStore: "badger-meta",
								ContentStore:  "local-content",
							},
							{
								Name:          "test-share", // Duplicate
								ExportPath:    "/data",
								MetadataStore: "badger-meta",
								ContentStore:  "local-content",
							},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "duplicate share name 'test-share'",
		},
		{
			name: "duplicate export paths",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "badger-meta", Type: "badger"},
							{Name: "local-content", Type: "local"},
						},
						Shares: []ShareConfig{
							{
								Name:          "share1",
								ExportPath:    "/export",
								MetadataStore: "badger-meta",
								ContentStore:  "local-content",
							},
							{
								Name:          "share2",
								ExportPath:    "/export", // Duplicate
								MetadataStore: "badger-meta",
								ContentStore:  "local-content",
							},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "duplicate export path '/export'",
		},
		{
			name: "same backend for metadata and content - should warn",
			dittoServer: &DittoServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: DittoServerSpec{
					Storage: StorageSpec{
						MetadataSize: "10Gi",
					},
					Config: DittoConfig{
						Backends: []BackendConfig{
							{Name: "badger-store", Type: "badger"},
						},
						Shares: []ShareConfig{
							{
								Name:          "test-share",
								ExportPath:    "/export",
								MetadataStore: "badger-store",
								ContentStore:  "badger-store", // Same backend
							},
						},
					},
				},
			},
			wantErr:     false,
			wantWarning: true,
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
