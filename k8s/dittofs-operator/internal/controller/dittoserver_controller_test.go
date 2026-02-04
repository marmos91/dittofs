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
			description: "Create DittoServer with SMB and user management using secrets",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("smb-with-users-server"),
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
								v1alpha1.WithUsers(&v1alpha1.UserManagementSpec{
									Users: []v1alpha1.UserSpec{
										{
											Username: "testuser",
											PasswordSecretRef: &corev1.SecretKeySelector{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: "user-credentials",
												},
												Key: "testuser-password-hash",
											},
											UID: 1001,
											GID: 1001,
											SharePermissions: map[string]string{
												"/": "read-write",
											},
										},
									},
									Groups: []v1alpha1.GroupSpec{
										{
											Name: "testgroup",
											GID:  1001,
											SharePermissions: map[string]string{
												"/": "read",
											},
										},
									},
									Guest: &v1alpha1.GuestSpec{
										Enabled: true,
										UID:     65534,
										GID:     65534,
										SharePermissions: map[string]string{
											"/": "read",
										},
									},
								}),
							),
						),
					)
				}(),
				secrets: []*corev1.Secret{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "user-credentials",
							Namespace: "default",
						},
						Data: map[string][]byte{
							"testuser-password-hash": []byte("$2y$10$rEKx.8vhUWJ1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c"), // bcrypt hash for "testpass"
						},
					},
				},
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "smb-with-users-server",
				},
			},
		},
		{
			description: "Create DittoServer with S3 backend and SMB using secrets",
			fields: fields{
				dittoServer: func() *v1alpha1.DittoServer {
					return v1alpha1.NewDittoServer(
						v1alpha1.WithName("s3-smb-secrets-server"),
						v1alpha1.WithNamespace("default"),
						v1alpha1.WithSpec(
							*v1alpha1.NewDittoServerSpec(
								v1alpha1.WithStorage(
									v1alpha1.StorageSpec{
										MetadataSize: "5Gi",
									},
								),
								v1alpha1.WithConfig(v1alpha1.DittoConfig{
									Backends: []v1alpha1.BackendConfig{
										{
											Name: "badger-metadata",
											Type: "badger",
											Config: map[string]string{
												"path": "/data/metadata",
											},
										},
										{
											Name: "s3-content",
											Type: "s3",
											Config: map[string]string{
												"bucket": "dittofs-bucket",
												"region": "us-east-1",
											},
											SecretRefs: map[string]corev1.SecretKeySelector{
												"access_key_id": {
													LocalObjectReference: corev1.LocalObjectReference{
														Name: "s3-credentials",
													},
													Key: "access-key-id",
												},
												"secret_access_key": {
													LocalObjectReference: corev1.LocalObjectReference{
														Name: "s3-credentials",
													},
													Key: "secret-access-key",
												},
											},
										},
									},
									Shares: []v1alpha1.ShareConfig{
										{
											Name:          "s3-share",
											ExportPath:    "/s3",
											MetadataStore: "badger-metadata",
											ContentStore:  "s3-content",
										},
									},
								}),
								v1alpha1.WithSMB(&v1alpha1.SMBAdapterSpec{
									Enabled: true,
								}),
								v1alpha1.WithUsers(&v1alpha1.UserManagementSpec{
									Users: []v1alpha1.UserSpec{
										{
											Username: "s3user",
											PasswordSecretRef: &corev1.SecretKeySelector{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: "user-credentials",
												},
												Key: "s3user-password-hash",
											},
											UID: 1002,
											GID: 1002,
											SharePermissions: map[string]string{
												"/s3": "read-write",
											},
										},
									},
								}),
							),
						),
					)
				}(),
				secrets: []*corev1.Secret{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "s3-credentials",
							Namespace: "default",
						},
						Data: map[string][]byte{
							"access-key-id":     []byte("access_key"),
							"secret-access-key": []byte("aws_secret_example"),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "user-credentials",
							Namespace: "default",
						},
						Data: map[string][]byte{
							"s3user-password-hash": []byte("$2y$10$anotherHashForS3User1234567890abcdefghijklmnopqr"),
						},
					},
				},
			},
			expectedStatus: &expectedStatus{
				phase:           "Pending",
				conditionReason: "StatefulSetNotReady",
				conditionStatus: metav1.ConditionFalse,
			},
			request: ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "s3-smb-secrets-server",
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
	service := &corev1.Service{}
	serviceKey := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.Name,
	}
	if err := r.Get(ctx, serviceKey, service); err != nil {
		t.Errorf("Failed to get Service: %v", err)
		return
	}

	hasNFS, hasMetrics, hasSMB := false, false, false
	for _, port := range service.Spec.Ports {
		if port.Name == "nfs" {
			hasNFS = true
		}
		if port.Name == "metrics" {
			hasMetrics = true
		}
		if port.Name == "smb" {
			hasSMB = true
		}
	}

	if !hasNFS {
		t.Errorf("Service missing NFS port")
	}
	if !hasMetrics {
		t.Errorf("Service missing metrics port")
	}

	// Check SMB port only if SMB is enabled
	if dittoServer != nil && dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled && !hasSMB {
		t.Errorf("Service missing SMB port when SMB is enabled")
	}

	// Check that SMB port is NOT present when SMB is disabled
	if (dittoServer == nil || dittoServer.Spec.SMB == nil || !dittoServer.Spec.SMB.Enabled) && hasSMB {
		t.Errorf("Service has SMB port when SMB is disabled")
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
