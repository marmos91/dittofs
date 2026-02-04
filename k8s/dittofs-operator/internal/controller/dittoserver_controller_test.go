/*
Copyright 2025.

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

package controller

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fields struct {
	dittoServer *v1alpha1.DittoServer
	configMap   *corev1.ConfigMap
	service     *corev1.Service
	statefulSet *appsv1.StatefulSet
	secrets     []*corev1.Secret
}

type expectedStatus struct {
	phase             string
	conditionReason   string
	conditionStatus   metav1.ConditionStatus
	availableReplicas *int32
}

func TestReconcileDittoServer(t *testing.T) {
	tests := []struct {
		description    string
		fields         fields
		request        ctrl.Request
		expectedStatus *expectedStatus
		wantErr        error
	}{
		{
			description: "Create DittoServer with all resources",
			fields: fields{
				dittoServer: v1alpha1.NewDittoServer(
					v1alpha1.WithName("hello"),
					v1alpha1.WithNamespace("default"),
					v1alpha1.WithSpec(
						*v1alpha1.NewDittoServerSpec(
							v1alpha1.WithStorage(
								v1alpha1.StorageSpec{
									MetadataSize: "10Gi",
									ContentSize:  "100Gi",
								},
							),
							v1alpha1.WithResources(
								corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										"cpu":    resource.MustParse("2"),
										"memory": resource.MustParse("4Gi"),
									},
									Requests: corev1.ResourceList{
										"cpu":    resource.MustParse("500m"),
										"memory": resource.MustParse("1Gi"),
									},
								},
							),
						),
					),
				),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "hello",
				},
			},
		},
		{
			description: "Create DittoServer with multiple replicas and no content storage",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					replicas := int32(3)
					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("multi-replica"),
						v1alpha1.WithNamespace("default"),
						v1alpha1.WithSpec(
							*v1alpha1.NewDittoServerSpec(
								v1alpha1.WithReplicas(&replicas),
								v1alpha1.WithStorage(
									v1alpha1.StorageSpec{
										MetadataSize: "5Gi",
									},
								),
							),
						),
					)
				}(),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "multi-replica",
				},
			},
		},
		{
			description: "Create DittoServer with replicas=0 (Stopped state)",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					replicas := int32(0)
					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("stopped-server"),
						v1alpha1.WithNamespace("default"),
						v1alpha1.WithSpec(
							*v1alpha1.NewDittoServerSpec(
								v1alpha1.WithReplicas(&replicas),
								v1alpha1.WithStorage(
									v1alpha1.StorageSpec{
										MetadataSize: "5Gi",
									},
								),
							),
						),
					)
				}(),
			},
			expectedStatus: &expectedStatus{
				phase:           "Stopped",
				conditionReason: "Stopped",
				conditionStatus: metav1.ConditionTrue,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "stopped-server",
				},
			},
		},
		{
			description: "DittoServer with ready StatefulSet",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					replicas := int32(2)
					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("ready-server"),
						v1alpha1.WithNamespace("default"),
						v1alpha1.WithSpec(
							*v1alpha1.NewDittoServerSpec(
								v1alpha1.WithReplicas(&replicas),
								v1alpha1.WithStorage(
									v1alpha1.StorageSpec{
										MetadataSize: "10Gi",
									},
								),
							),
						),
					)
				}(),
				statefulSet: func() *appsv1.StatefulSet {
					replicas := int32(2)
					return &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "ready-server",
							Namespace: "default",
						},
						Spec: appsv1.StatefulSetSpec{
							Replicas: &replicas,
						},
						Status: appsv1.StatefulSetStatus{
							ReadyReplicas: 2,
						},
					}
				}(),
			},
			expectedStatus: &expectedStatus{
				phase:             "Running",
				conditionReason:   "StatefulSetReady",
				conditionStatus:   metav1.ConditionTrue,
				availableReplicas: func() *int32 { r := int32(2); return &r }(),
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "ready-server",
				},
			},
		},
		{
			description: "Create DittoServer with SMB enabled (default port)",
			fields: fields{
				dittoServer: v1alpha1.NewDittoServer(
					v1alpha1.WithName("smb-enabled-server"),
					v1alpha1.WithNamespace("default"),
					v1alpha1.WithSpec(
						*v1alpha1.NewDittoServerSpec(
							v1alpha1.WithStorage(
								v1alpha1.StorageSpec{
									MetadataSize: "5Gi",
								},
							),
							v1alpha1.WithSMB(&v1alpha1.SMBAdapterSpec{
								Enabled: true,
							}),
						),
					),
				),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "smb-enabled-server",
				},
			},
		},
		{
			description: "Create DittoServer with SMB enabled and custom port",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					customPort := int32(8445)
					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("smb-custom-port-server"),
						v1alpha1.WithNamespace("default"),
						v1alpha1.WithSpec(
							*v1alpha1.NewDittoServerSpec(
								v1alpha1.WithStorage(
									v1alpha1.StorageSpec{
										MetadataSize: "5Gi",
									},
								),
								v1alpha1.WithSMB(&v1alpha1.SMBAdapterSpec{
									Enabled: true,
									Port:    &customPort,
								}),
							),
						),
					)
				}(),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "smb-custom-port-server",
				},
			},
		},
		{
			description: "Create DittoServer with SMB disabled explicitly",
			fields: fields{
				dittoServer: v1alpha1.NewDittoServer(
					v1alpha1.WithName("smb-disabled-server"),
					v1alpha1.WithNamespace("default"),
					v1alpha1.WithSpec(
						*v1alpha1.NewDittoServerSpec(
							v1alpha1.WithStorage(
								v1alpha1.StorageSpec{
									MetadataSize: "5Gi",
								},
							),
							v1alpha1.WithSMB(&v1alpha1.SMBAdapterSpec{
								Enabled: false,
							}),
						),
					),
				),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "smb-disabled-server",
				},
			},
		},
		{
			description: "Create DittoServer with SMB enabled and comprehensive configuration",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					customPort := int32(9445)
					maxConnections := int32(100)
					minGrant := int32(16)
					maxGrant := int32(8192)
					initialGrant := int32(256)
					maxSessionCredits := int32(65535)
					loadThresholdHigh := int32(1000)
					loadThresholdLow := int32(100)
					aggressiveClientThreshold := int32(256)

					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("smb-full-config-server"),
						v1alpha1.WithNamespace("default"),
						v1alpha1.WithSpec(
							*v1alpha1.NewDittoServerSpec(
								v1alpha1.WithStorage(
									v1alpha1.StorageSpec{
										MetadataSize: "5Gi",
									},
								),
								v1alpha1.WithSMB(&v1alpha1.SMBAdapterSpec{
									Enabled:        true,
									Port:           &customPort,
									MaxConnections: &maxConnections,
									Timeouts: &v1alpha1.SMBTimeoutsSpec{
										Read:     "60s",
										Write:    "60s",
										Idle:     "300s",
										Shutdown: "30s",
									},
									Credits: &v1alpha1.SMBCreditsSpec{
										MinGrant:                  &minGrant,
										MaxGrant:                  &maxGrant,
										InitialGrant:              &initialGrant,
										MaxSessionCredits:         &maxSessionCredits,
										LoadThresholdHigh:         &loadThresholdHigh,
										LoadThresholdLow:          &loadThresholdLow,
										AggressiveClientThreshold: &aggressiveClientThreshold,
									},
								}),
							),
						),
					)
				}(),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "smb-full-config-server",
				},
			},
		},
		{
			description: "Create DittoServer with database and cache config",
			fields: fields{
				dittoServer: v1alpha1.NewDittoServer(
					v1alpha1.WithName("db-cache-server"),
					v1alpha1.WithNamespace("default"),
					v1alpha1.WithSpec(
						*v1alpha1.NewDittoServerSpec(
							v1alpha1.WithStorage(
								v1alpha1.StorageSpec{
									MetadataSize: "5Gi",
								},
							),
							v1alpha1.WithDatabase(&v1alpha1.DatabaseConfig{
								Type: "sqlite",
								SQLite: &v1alpha1.SQLiteConfig{
									Path: "/data/controlplane/controlplane.db",
								},
							}),
							v1alpha1.WithCache(&v1alpha1.InfraCacheConfig{
								Path: "/data/cache",
								Size: "1GB",
							}),
						),
					),
				),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "db-cache-server",
				},
			},
		},
		{
			description: "Create DittoServer with metrics enabled",
			fields: fields{
				dittoServer: v1alpha1.NewDittoServer(
					v1alpha1.WithName("metrics-server"),
					v1alpha1.WithNamespace("default"),
					v1alpha1.WithSpec(
						*v1alpha1.NewDittoServerSpec(
							v1alpha1.WithStorage(
								v1alpha1.StorageSpec{
									MetadataSize: "5Gi",
								},
							),
							v1alpha1.WithMetrics(&v1alpha1.MetricsConfig{
								Enabled: true,
								Port:    9090,
							}),
							v1alpha1.WithControlPlane(&v1alpha1.ControlPlaneAPIConfig{
								Port: 8080,
							}),
						),
					),
				),
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "metrics-server",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			ctx := context.TODO()
			r := setupDittoServerReconciler(t, tt.fields)

			_, err := r.Reconcile(ctx, tt.request)
			if err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			verifyConfigMap(t, ctx, r, tt.request)
			verifyService(t, ctx, r, tt.request, tt.fields.dittoServer)
			verifyStatefulSet(t, ctx, r, tt.request, tt.fields.dittoServer)
			verifyDittoServerStatus(t, ctx, r, tt.request, tt.expectedStatus)
		})
	}
}

func verifyConfigMap(t *testing.T, ctx context.Context, r *DittoServerReconciler, req ctrl.Request) {
	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name + "-config",
	}
	if err := r.Get(ctx, configMapKey, configMap); err != nil {
		t.Errorf("Failed to get ConfigMap: %v", err)
		return
	}

	if _, ok := configMap.Data["config.yaml"]; !ok {
		t.Errorf("ConfigMap missing config.yaml key")
	}
}

func verifyService(t *testing.T, ctx context.Context, r *DittoServerReconciler, req ctrl.Request, dittoServer *v1alpha1.DittoServer) {
	// Verify headless service exists
	headlessService := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name + "-headless",
	}, headlessService); err != nil {
		t.Errorf("Failed to get headless Service: %v", err)
	} else {
		if headlessService.Spec.ClusterIP != corev1.ClusterIPNone {
			t.Errorf("Headless service should have ClusterIP: None")
		}
	}

	// Verify file service exists with NFS port
	fileService := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name + "-file",
	}, fileService); err != nil {
		t.Errorf("Failed to get file Service: %v", err)
	} else {
		hasNFS, hasSMB := false, false
		for _, port := range fileService.Spec.Ports {
			if port.Name == "nfs" {
				hasNFS = true
			}
			if port.Name == "smb" {
				hasSMB = true
			}
		}
		if !hasNFS {
			t.Errorf("File service missing NFS port")
		}
		// Check SMB port only if SMB is enabled
		if dittoServer != nil && dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled && !hasSMB {
			t.Errorf("File service missing SMB port when SMB is enabled")
		}
		// Check that SMB port is NOT present when SMB is disabled
		if (dittoServer == nil || dittoServer.Spec.SMB == nil || !dittoServer.Spec.SMB.Enabled) && hasSMB {
			t.Errorf("File service has SMB port when SMB is disabled")
		}
	}

	// Verify API service exists
	apiService := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name + "-api",
	}, apiService); err != nil {
		t.Errorf("Failed to get API Service: %v", err)
	} else {
		hasAPI := false
		for _, port := range apiService.Spec.Ports {
			if port.Name == "api" {
				hasAPI = true
			}
		}
		if !hasAPI {
			t.Errorf("API service missing api port")
		}
	}

	// Verify metrics service exists only if metrics enabled
	metricsService := &corev1.Service{}
	metricsEnabled := dittoServer != nil && dittoServer.Spec.Metrics != nil && dittoServer.Spec.Metrics.Enabled
	err := r.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name + "-metrics",
	}, metricsService)

	if metricsEnabled {
		if err != nil {
			t.Errorf("Failed to get metrics Service when metrics enabled: %v", err)
		}
	} else {
		// Metrics service should not exist when disabled (or be deleted)
		// Note: In test context, service might not exist which is fine
	}
}

func verifyStatefulSet(t *testing.T, ctx context.Context, r *DittoServerReconciler, req ctrl.Request, dittoServer *v1alpha1.DittoServer) {
	statefulSet := &appsv1.StatefulSet{}
	statefulSetKey := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name,
	}
	if err := r.Get(ctx, statefulSetKey, statefulSet); err != nil {
		t.Errorf("Failed to get StatefulSet: %v", err)
		return
	}

	expectedReplicas := int32(1)
	if dittoServer.Spec.Replicas != nil {
		expectedReplicas = *dittoServer.Spec.Replicas
	}
	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != expectedReplicas {
		t.Errorf("Expected %d replicas, got %v", expectedReplicas, statefulSet.Spec.Replicas)
	}

	if len(statefulSet.Spec.VolumeClaimTemplates) < 1 {
		t.Errorf("Expected at least 1 volume claim template (metadata), got %d", len(statefulSet.Spec.VolumeClaimTemplates))
	}

	hasMetadata := false
	for _, vct := range statefulSet.Spec.VolumeClaimTemplates {
		if vct.Name == "metadata" {
			hasMetadata = true
			expectedSize := dittoServer.Spec.Storage.MetadataSize
			if storage, ok := vct.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				if storage.String() != expectedSize {
					t.Errorf("Expected metadata storage %s, got %s", expectedSize, storage.String())
				}
			}
		}
	}
	if !hasMetadata {
		t.Errorf("StatefulSet missing metadata volume claim template")
	}

	if len(statefulSet.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("Expected 1 container, got %d", len(statefulSet.Spec.Template.Spec.Containers))
		return
	}

	container := statefulSet.Spec.Template.Spec.Containers[0]
	if container.Name != "dittofs" {
		t.Errorf("Expected container name 'dittofs', got %s", container.Name)
	}

	if dittoServer.Spec.Resources.Limits != nil {
		if !container.Resources.Limits.Cpu().Equal(*dittoServer.Spec.Resources.Limits.Cpu()) {
			t.Errorf("CPU limit mismatch")
		}
		if !container.Resources.Limits.Memory().Equal(*dittoServer.Spec.Resources.Limits.Memory()) {
			t.Errorf("Memory limit mismatch")
		}
	}
}

func verifyDittoServerStatus(t *testing.T, ctx context.Context, r *DittoServerReconciler, req ctrl.Request, expected *expectedStatus) {
	dittoServer := &v1alpha1.DittoServer{}
	dittoServerKey := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name,
	}
	if err := r.Get(ctx, dittoServerKey, dittoServer); err != nil {
		t.Errorf("Failed to get DittoServer: %v", err)
		return
	}

	if dittoServer.Status.Phase == "" {
		t.Errorf("DittoServer status phase is empty")
	}

	if expected != nil && dittoServer.Status.Phase != expected.phase {
		t.Errorf("Expected Phase '%s', got '%s'", expected.phase, dittoServer.Status.Phase)
	}

	if dittoServer.Status.NFSEndpoint == "" {
		t.Errorf("DittoServer status NFSEndpoint is empty")
	}

	if len(dittoServer.Status.Conditions) == 0 {
		t.Errorf("DittoServer status has no conditions")
		return
	}

	readyCondition := findCondition(dittoServer.Status.Conditions, "Ready")
	if readyCondition == nil {
		t.Errorf("Ready condition not found")
		return
	}

	if expected != nil {
		if readyCondition.Reason != expected.conditionReason {
			t.Errorf("Expected Ready condition reason '%s', got '%s'", expected.conditionReason, readyCondition.Reason)
		}
		if readyCondition.Status != expected.conditionStatus {
			t.Errorf("Expected Ready condition status %s, got %s", expected.conditionStatus, readyCondition.Status)
		}
		if expected.availableReplicas != nil && dittoServer.Status.AvailableReplicas != *expected.availableReplicas {
			t.Errorf("Expected %d available replicas, got %d", *expected.availableReplicas, dittoServer.Status.AvailableReplicas)
		}
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func setupDittoServerReconciler(t *testing.T, fields fields) *DittoServerReconciler {
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add v1alpha1 scheme: %v", err)
	}

	var objs []runtime.Object
	if fields.dittoServer != nil {
		objs = append(objs, fields.dittoServer)
	}
	if fields.configMap != nil {
		objs = append(objs, fields.configMap)
	}
	if fields.service != nil {
		objs = append(objs, fields.service)
	}
	if fields.statefulSet != nil {
		objs = append(objs, fields.statefulSet)
	}
	for _, secret := range fields.secrets {
		if secret != nil {
			objs = append(objs, secret)
		}
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&v1alpha1.DittoServer{}).
		Build()

	return &DittoServerReconciler{
		Client: fakeClient,
		Scheme: s,
	}
}
