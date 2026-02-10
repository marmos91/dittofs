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
	"fmt"
	"testing"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newAdapterService creates an adapter Service for testing.
func newAdapterService(crName, namespace, adapterType string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adapterResourceName(crName, adapterType),
			Namespace: namespace,
			Labels:    adapterServiceLabels(crName, adapterType),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{
				"app":      "dittofs-server",
				"instance": crName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       adapterType,
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// newStaticService creates a static Service (without adapter labels) for testing.
func newStaticService(name, namespace, crName string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":      "dittofs-server",
				"instance": crName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app":      "dittofs-server",
				"instance": crName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "api",
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// listAdapterServices returns all adapter Services for a given CR using the adapter label.
func listAdapterServices(t *testing.T, r *DittoServerReconciler, namespace, crName string) []corev1.Service {
	t.Helper()
	var svcList corev1.ServiceList
	err := r.List(context.Background(), &svcList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			adapterServiceLabel: "true",
			"instance":          crName,
		},
	)
	if err != nil {
		t.Fatalf("Failed to list adapter services: %v", err)
	}
	return svcList.Items
}

func TestReconcileAdapterServices_NilAdapters_Skips(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create a static -file Service to verify it is NOT deleted.
	staticSvc := newStaticService("test-server-file", "default", "test-server", 12049)

	r := setupAuthReconciler(t, ds, staticSvc)

	// No lastKnownAdapters set (nil = no poll yet)

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify no adapter Services created.
	adapterSvcs := listAdapterServices(t, r, "default", "test-server")
	if len(adapterSvcs) != 0 {
		t.Errorf("Expected 0 adapter services, got %d", len(adapterSvcs))
	}

	// Verify static Service still exists.
	staticCheck := &corev1.Service{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-server-file",
	}, staticCheck)
	if err != nil {
		t.Errorf("Static Service should still exist: %v", err)
	}
}

func TestReconcileAdapterServices_EmptyAdapters_DeletesOrphans(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create an adapter Service that should be deleted.
	orphanSvc := newAdapterService("test-server", "default", "nfs", 12049)

	r := setupAuthReconciler(t, ds, orphanSvc)

	// Set empty adapter list (legitimate -- all adapters stopped).
	r.setLastKnownAdapters(ds, []AdapterInfo{})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify the orphan adapter Service was deleted.
	adapterSvcs := listAdapterServices(t, r, "default", "test-server")
	if len(adapterSvcs) != 0 {
		t.Errorf("Expected 0 adapter services after cleanup, got %d", len(adapterSvcs))
	}
}

func TestReconcileAdapterServices_CreateServices(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// Set adapters: NFS and SMB both enabled+running.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
		{Type: "smb", Enabled: true, Running: true, Port: 12445},
	})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify two adapter Services were created.
	adapterSvcs := listAdapterServices(t, r, "default", "test-server")
	if len(adapterSvcs) != 2 {
		t.Fatalf("Expected 2 adapter services, got %d", len(adapterSvcs))
	}

	// Verify Services have correct names and ports.
	svcByType := make(map[string]*corev1.Service)
	for i := range adapterSvcs {
		svc := &adapterSvcs[i]
		adapterType := svc.Labels[adapterTypeLabel]
		svcByType[adapterType] = svc
	}

	// Check NFS Service.
	nfsSvc, ok := svcByType["nfs"]
	if !ok {
		t.Fatal("NFS adapter Service not found")
	}
	if nfsSvc.Name != "test-server-adapter-nfs" {
		t.Errorf("Expected NFS service name 'test-server-adapter-nfs', got %s", nfsSvc.Name)
	}
	if len(nfsSvc.Spec.Ports) != 1 || nfsSvc.Spec.Ports[0].Port != 12049 {
		t.Errorf("Expected NFS port 12049, got %v", nfsSvc.Spec.Ports)
	}
	if nfsSvc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Expected NFS service type LoadBalancer, got %s", nfsSvc.Spec.Type)
	}

	// Check SMB Service.
	smbSvc, ok := svcByType["smb"]
	if !ok {
		t.Fatal("SMB adapter Service not found")
	}
	if smbSvc.Name != "test-server-adapter-smb" {
		t.Errorf("Expected SMB service name 'test-server-adapter-smb', got %s", smbSvc.Name)
	}
	if len(smbSvc.Spec.Ports) != 1 || smbSvc.Spec.Ports[0].Port != 12445 {
		t.Errorf("Expected SMB port 12445, got %v", smbSvc.Spec.Ports)
	}

	// Check owner references.
	for _, svc := range adapterSvcs {
		if len(svc.OwnerReferences) == 0 {
			t.Errorf("Service %s has no owner references", svc.Name)
		} else if svc.OwnerReferences[0].Kind != "DittoServer" {
			t.Errorf("Service %s owner reference kind = %s, want DittoServer", svc.Name, svc.OwnerReferences[0].Kind)
		}
	}

	// Check labels.
	for _, svc := range adapterSvcs {
		if svc.Labels[adapterServiceLabel] != "true" {
			t.Errorf("Service %s missing adapter-service label", svc.Name)
		}
		if svc.Labels["app"] != "dittofs-server" {
			t.Errorf("Service %s missing app label", svc.Name)
		}
		if svc.Labels["instance"] != "test-server" {
			t.Errorf("Service %s missing instance label", svc.Name)
		}
	}
}

func TestReconcileAdapterServices_DeleteStoppedAdapter(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create NFS adapter Service.
	nfsSvc := newAdapterService("test-server", "default", "nfs", 12049)

	r := setupAuthReconciler(t, ds, nfsSvc)

	// Set empty adapters (all adapters removed).
	r.setLastKnownAdapters(ds, []AdapterInfo{})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify NFS Service was deleted.
	adapterSvcs := listAdapterServices(t, r, "default", "test-server")
	if len(adapterSvcs) != 0 {
		t.Errorf("Expected 0 adapter services after deletion, got %d", len(adapterSvcs))
	}
}

func TestReconcileAdapterServices_UpdatePortChange(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create NFS adapter Service with old port 12049.
	nfsSvc := newAdapterService("test-server", "default", "nfs", 12049)

	r := setupAuthReconciler(t, ds, nfsSvc)

	// Set adapters with NFS port changed to 2049.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 2049},
	})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify the Service was updated with new port.
	updated := &corev1.Service{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-server-adapter-nfs",
	}, updated)
	if err != nil {
		t.Fatalf("Failed to get updated Service: %v", err)
	}

	if len(updated.Spec.Ports) != 1 {
		t.Fatalf("Expected 1 port, got %d", len(updated.Spec.Ports))
	}
	if updated.Spec.Ports[0].Port != 2049 {
		t.Errorf("Expected port 2049, got %d", updated.Spec.Ports[0].Port)
	}
	if updated.Spec.Ports[0].TargetPort.IntVal != 2049 {
		t.Errorf("Expected targetPort 2049, got %d", updated.Spec.Ports[0].TargetPort.IntVal)
	}
}

func TestReconcileAdapterServices_StaticServicesUntouched(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create static Services without the adapter-service label.
	headlessSvc := newStaticService("test-server-headless", "default", "test-server", 12049)
	fileSvc := newStaticService("test-server-file", "default", "test-server", 12049)
	apiSvc := newStaticService("test-server-api", "default", "test-server", 8080)
	metricsSvc := newStaticService("test-server-metrics", "default", "test-server", 9090)

	r := setupAuthReconciler(t, ds, headlessSvc, fileSvc, apiSvc, metricsSvc)

	// Set empty adapter list so the reconciler would try to delete orphaned adapter Services.
	r.setLastKnownAdapters(ds, []AdapterInfo{})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify all 4 static Services still exist.
	staticNames := []string{
		"test-server-headless",
		"test-server-file",
		"test-server-api",
		"test-server-metrics",
	}
	for _, name := range staticNames {
		svc := &corev1.Service{}
		err := r.Get(context.Background(), client.ObjectKey{
			Namespace: "default",
			Name:      name,
		}, svc)
		if err != nil {
			t.Errorf("Static Service %s should still exist: %v", name, err)
		}
	}
}

func TestReconcileAdapterServices_OnlyEnabledRunning(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// NFS enabled+running, SMB enabled but NOT running.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
		{Type: "smb", Enabled: true, Running: false, Port: 12445},
	})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify only NFS Service created (not SMB).
	adapterSvcs := listAdapterServices(t, r, "default", "test-server")
	if len(adapterSvcs) != 1 {
		t.Fatalf("Expected 1 adapter service (NFS only), got %d", len(adapterSvcs))
	}
	if adapterSvcs[0].Labels[adapterTypeLabel] != "nfs" {
		t.Errorf("Expected adapter type 'nfs', got '%s'", adapterSvcs[0].Labels[adapterTypeLabel])
	}
}

func TestReconcileAdapterServices_CustomServiceType(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterServices = &dittoiov1alpha1.AdapterServiceConfig{
		Type: "NodePort",
	}

	r := setupAuthReconciler(t, ds)

	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
	})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify the created Service has type NodePort.
	svc := &corev1.Service{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-server-adapter-nfs",
	}, svc)
	if err != nil {
		t.Fatalf("Failed to get adapter service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("Expected ServiceType NodePort, got %s", svc.Spec.Type)
	}
}

func TestReconcileAdapterServices_CustomAnnotations(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterServices = &dittoiov1alpha1.AdapterServiceConfig{
		Annotations: map[string]string{
			"service.beta.kubernetes.io/aws-load-balancer-type": "nlb",
		},
	}

	r := setupAuthReconciler(t, ds)

	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
	})

	err := r.reconcileAdapterServices(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapterServices returned error: %v", err)
	}

	// Verify the created Service has the custom annotation.
	svc := &corev1.Service{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-server-adapter-nfs",
	}, svc)
	if err != nil {
		t.Fatalf("Failed to get adapter service: %v", err)
	}
	if svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-type"] != "nlb" {
		t.Errorf("Expected aws-load-balancer-type annotation 'nlb', got '%s'",
			svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-type"])
	}
}

func TestGetAdapterServiceType_Default(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	// No adapterServices spec.

	svcType := getAdapterServiceType(ds)
	if svcType != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Expected default ServiceType LoadBalancer, got %s", svcType)
	}
}

func TestGetAdapterServiceType_Custom(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterServices = &dittoiov1alpha1.AdapterServiceConfig{
		Type: "ClusterIP",
	}

	svcType := getAdapterServiceType(ds)
	if svcType != corev1.ServiceTypeClusterIP {
		t.Errorf("Expected ServiceType ClusterIP, got %s", svcType)
	}
}

// newTestStatefulSet creates a minimal StatefulSet for testing container port reconciliation.
func newTestStatefulSet(name, namespace string, ports []corev1.ContainerPort) *appsv1.StatefulSet {
	replicas := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name + "-headless",
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":      "dittofs-server",
					"instance": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":      "dittofs-server",
						"instance": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "dittofs",
							Image: "dittofs:latest",
							Ports: ports,
						},
					},
				},
			},
		},
	}
}

func TestReconcileContainerPorts_AddsAdapterPorts(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create StatefulSet with static ports only.
	staticPorts := []corev1.ContainerPort{
		{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
		{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
	}
	sts := newTestStatefulSet("test-server", "default", staticPorts)

	r := setupAuthReconciler(t, ds, sts)

	// Active adapters: NFS running.
	activeAdapters := map[string]AdapterInfo{
		"nfs": {Type: "nfs", Enabled: true, Running: true, Port: 12049},
	}

	err := r.reconcileContainerPorts(context.Background(), ds, activeAdapters)
	if err != nil {
		t.Fatalf("reconcileContainerPorts returned error: %v", err)
	}

	// Verify StatefulSet was updated with adapter-nfs port alongside static ports.
	updated := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: "test-server",
	}, updated); err != nil {
		t.Fatalf("Failed to get StatefulSet: %v", err)
	}

	ports := updated.Spec.Template.Spec.Containers[0].Ports
	if len(ports) != 3 {
		t.Fatalf("Expected 3 ports (nfs, api, adapter-nfs), got %d: %v", len(ports), portNames(ports))
	}

	// Verify both static "nfs" and dynamic "adapter-nfs" coexist (Phase 3 coexistence).
	portMap := make(map[string]int32)
	for _, p := range ports {
		portMap[p.Name] = p.ContainerPort
	}

	if portMap["nfs"] != 12049 {
		t.Errorf("Static nfs port should be 12049, got %d", portMap["nfs"])
	}
	if portMap["api"] != 8080 {
		t.Errorf("Static api port should be 8080, got %d", portMap["api"])
	}
	if portMap["adapter-nfs"] != 12049 {
		t.Errorf("Dynamic adapter-nfs port should be 12049, got %d", portMap["adapter-nfs"])
	}
}

func TestReconcileContainerPorts_RemovesStoppedAdapterPorts(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create StatefulSet with static ports AND an existing dynamic adapter-smb port.
	ports := []corev1.ContainerPort{
		{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
		{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "adapter-smb", ContainerPort: 12445, Protocol: corev1.ProtocolTCP},
	}
	sts := newTestStatefulSet("test-server", "default", ports)

	r := setupAuthReconciler(t, ds, sts)

	// Active adapters: only NFS (no SMB).
	activeAdapters := map[string]AdapterInfo{
		"nfs": {Type: "nfs", Enabled: true, Running: true, Port: 12049},
	}

	err := r.reconcileContainerPorts(context.Background(), ds, activeAdapters)
	if err != nil {
		t.Fatalf("reconcileContainerPorts returned error: %v", err)
	}

	// Verify adapter-smb was removed and adapter-nfs was added.
	updated := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: "test-server",
	}, updated); err != nil {
		t.Fatalf("Failed to get StatefulSet: %v", err)
	}

	updatedPorts := updated.Spec.Template.Spec.Containers[0].Ports
	portMap := make(map[string]int32)
	for _, p := range updatedPorts {
		portMap[p.Name] = p.ContainerPort
	}

	// Static ports preserved.
	if portMap["nfs"] != 12049 {
		t.Errorf("Static nfs port should be preserved, got %d", portMap["nfs"])
	}
	if portMap["api"] != 8080 {
		t.Errorf("Static api port should be preserved, got %d", portMap["api"])
	}

	// adapter-nfs added.
	if portMap["adapter-nfs"] != 12049 {
		t.Errorf("Expected adapter-nfs port 12049, got %d", portMap["adapter-nfs"])
	}

	// adapter-smb removed.
	if _, exists := portMap["adapter-smb"]; exists {
		t.Error("adapter-smb port should have been removed")
	}
}

func TestReconcileContainerPorts_NoChange_NoUpdate(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create StatefulSet with static ports AND matching dynamic adapter-nfs port.
	ports := []corev1.ContainerPort{
		{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
		{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "adapter-nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
	}
	sts := newTestStatefulSet("test-server", "default", ports)

	r := setupAuthReconciler(t, ds, sts)

	// Get initial ResourceVersion.
	initialSts := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: "test-server",
	}, initialSts); err != nil {
		t.Fatalf("Failed to get initial StatefulSet: %v", err)
	}
	initialRV := initialSts.ResourceVersion

	// Active adapters match existing ports exactly.
	activeAdapters := map[string]AdapterInfo{
		"nfs": {Type: "nfs", Enabled: true, Running: true, Port: 12049},
	}

	err := r.reconcileContainerPorts(context.Background(), ds, activeAdapters)
	if err != nil {
		t.Fatalf("reconcileContainerPorts returned error: %v", err)
	}

	// Verify ResourceVersion hasn't changed (no update was made).
	afterSts := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: "test-server",
	}, afterSts); err != nil {
		t.Fatalf("Failed to get StatefulSet after reconcile: %v", err)
	}

	if afterSts.ResourceVersion != initialRV {
		t.Errorf("StatefulSet should not have been updated (no port change). RV before=%s, after=%s",
			initialRV, afterSts.ResourceVersion)
	}
}

func TestReconcileContainerPorts_StatefulSetNotFound(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// No StatefulSet registered in fake client.
	r := setupAuthReconciler(t, ds)

	activeAdapters := map[string]AdapterInfo{
		"nfs": {Type: "nfs", Enabled: true, Running: true, Port: 12049},
	}

	// Should return nil (graceful skip).
	err := r.reconcileContainerPorts(context.Background(), ds, activeAdapters)
	if err != nil {
		t.Errorf("Expected nil error when StatefulSet not found, got: %v", err)
	}
}

func TestReconcileContainerPorts_StaticPortsPreserved(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create StatefulSet with all static ports, no adapter- ports.
	ports := []corev1.ContainerPort{
		{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
		{Name: "smb", ContainerPort: 12445, Protocol: corev1.ProtocolTCP},
		{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
	}
	sts := newTestStatefulSet("test-server", "default", ports)

	r := setupAuthReconciler(t, ds, sts)

	// Get initial ResourceVersion.
	initialSts := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: "test-server",
	}, initialSts); err != nil {
		t.Fatalf("Failed to get initial StatefulSet: %v", err)
	}
	initialRV := initialSts.ResourceVersion

	// Empty active adapters -- no dynamic ports to add.
	activeAdapters := map[string]AdapterInfo{}

	err := r.reconcileContainerPorts(context.Background(), ds, activeAdapters)
	if err != nil {
		t.Fatalf("reconcileContainerPorts returned error: %v", err)
	}

	// Verify StatefulSet was NOT updated (no adapter-* ports to begin with, no change).
	afterSts := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: "test-server",
	}, afterSts); err != nil {
		t.Fatalf("Failed to get StatefulSet: %v", err)
	}

	if afterSts.ResourceVersion != initialRV {
		t.Errorf("StatefulSet should not have been updated (static ports only, no dynamic changes)")
	}

	// Verify all static ports are still present.
	afterPorts := afterSts.Spec.Template.Spec.Containers[0].Ports
	if len(afterPorts) != 4 {
		t.Errorf("Expected 4 static ports, got %d", len(afterPorts))
	}
}

func TestPortsEqual(t *testing.T) {
	tests := []struct {
		name     string
		a, b     []corev1.ContainerPort
		expected bool
	}{
		{
			name: "same ports same order",
			a: []corev1.ContainerPort{
				{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			b: []corev1.ContainerPort{
				{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			expected: true,
		},
		{
			name: "same ports different order (sorted before comparison)",
			a: []corev1.ContainerPort{
				{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			b: []corev1.ContainerPort{
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
				{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			},
			expected: false, // portsEqual compares element-by-element; caller must sort first
		},
		{
			name: "different lengths",
			a: []corev1.ContainerPort{
				{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			},
			b: []corev1.ContainerPort{
				{Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			expected: false,
		},
		{
			name: "same length different port numbers",
			a: []corev1.ContainerPort{
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			b: []corev1.ContainerPort{
				{Name: "nfs", ContainerPort: 2049, Protocol: corev1.ProtocolTCP},
			},
			expected: false,
		},
		{
			name:     "empty slices",
			a:        []corev1.ContainerPort{},
			b:        []corev1.ContainerPort{},
			expected: true,
		},
		{
			name:     "both nil",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name: "different protocols",
			a: []corev1.ContainerPort{
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			b: []corev1.ContainerPort{
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolUDP},
			},
			expected: false,
		},
		{
			name: "different names same port",
			a: []corev1.ContainerPort{
				{Name: "nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			b: []corev1.ContainerPort{
				{Name: "adapter-nfs", ContainerPort: 12049, Protocol: corev1.ProtocolTCP},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := portsEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("portsEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// portNames is a test helper that extracts port names from a slice of container ports.
func portNames(ports []corev1.ContainerPort) []string {
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = fmt.Sprintf("%s:%d", p.Name, p.ContainerPort)
	}
	return names
}
