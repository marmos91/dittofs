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

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// metricsEnabledDittoServer returns a DittoServer with metrics enabled.
func metricsEnabledDittoServer(name, namespace string, sm *dittoiov1alpha1.ServiceMonitorSpec) *dittoiov1alpha1.DittoServer {
	ds := newTestDittoServer(name, namespace)
	ds.Spec.Metrics = &dittoiov1alpha1.MetricsSpec{
		Enabled:        true,
		ServiceMonitor: sm,
	}
	return ds
}

// setupMetricsReconciler builds a reconciler whose RESTMapper either knows the
// ServiceMonitor GVK (smCRDPresent=true) or returns NoMatch for it
// (smCRDPresent=false), so the CRD-discovery gate can be exercised.
func setupMetricsReconciler(t *testing.T, smCRDPresent bool, objs ...runtime.Object) *DittoServerReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := dittoiov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add v1alpha1 scheme: %v", err)
	}

	// Build a RESTMapper covering the core/v1alpha1 types via the scheme.
	mapper := apimeta.NewDefaultRESTMapper(nil)
	for gvk := range s.AllKnownTypes() {
		mapper.Add(gvk, apimeta.RESTScopeNamespace)
	}
	if smCRDPresent {
		mapper.Add(serviceMonitorGVK, apimeta.RESTScopeNamespace)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithRESTMapper(mapper).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&dittoiov1alpha1.DittoServer{}).
		Build()

	return &DittoServerReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
	}
}

func getMetricsService(t *testing.T, r *DittoServerReconciler, ds *dittoiov1alpha1.DittoServer) (*corev1.Service, bool) {
	t.Helper()
	svc := &corev1.Service{}
	err := r.Get(context.Background(), client.ObjectKey{
		Namespace: ds.Namespace,
		Name:      metricsServiceName(ds.Name),
	}, svc)
	if err != nil {
		return nil, false
	}
	return svc, true
}

func getServiceMonitor(t *testing.T, r *DittoServerReconciler, ds *dittoiov1alpha1.DittoServer) (*unstructured.Unstructured, bool) {
	t.Helper()
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	err := r.Get(context.Background(), client.ObjectKey{
		Namespace: ds.Namespace,
		Name:      metricsServiceName(ds.Name),
	}, sm)
	if err != nil {
		return nil, false
	}
	return sm, true
}

func TestReconcileMetrics_Disabled_NoService(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	r := setupMetricsReconciler(t, false, ds)

	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics returned error when disabled: %v", err)
	}
	if _, ok := getMetricsService(t, r, ds); ok {
		t.Errorf("metrics Service should not exist when metrics are disabled")
	}
}

func TestReconcileMetrics_Enabled_ServiceWithScrapeAnnotations(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", nil)
	r := setupMetricsReconciler(t, false, ds)

	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics returned error: %v", err)
	}

	svc, ok := getMetricsService(t, r, ds)
	if !ok {
		t.Fatalf("metrics Service was not created")
	}

	if got := svc.Annotations[prometheusScrapeAnnotation]; got != "true" {
		t.Errorf("expected %s=true, got %q", prometheusScrapeAnnotation, got)
	}
	if got := svc.Annotations[prometheusPortAnnotation]; got != "9090" {
		t.Errorf("expected %s=9090, got %q", prometheusPortAnnotation, got)
	}
	if got := svc.Annotations[prometheusPathAnnotation]; got != "/metrics" {
		t.Errorf("expected %s=/metrics, got %q", prometheusPathAnnotation, got)
	}

	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != metricsPortName || svc.Spec.Ports[0].Port != 9090 {
		t.Errorf("expected single %q port 9090, got %+v", metricsPortName, svc.Spec.Ports)
	}
}

func TestReconcileMetrics_ServiceMonitor_CRDPresent_Created(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", &dittoiov1alpha1.ServiceMonitorSpec{
		Enabled:  true,
		Interval: "30s",
		Labels:   map[string]string{"release": "kube-prometheus-stack"},
	})
	r := setupMetricsReconciler(t, true, ds)

	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics returned error: %v", err)
	}

	sm, ok := getServiceMonitor(t, r, ds)
	if !ok {
		t.Fatalf("ServiceMonitor was not created when CRD is present")
	}

	if got := sm.GetLabels()["release"]; got != "kube-prometheus-stack" {
		t.Errorf("expected user label release=kube-prometheus-stack, got %q", got)
	}

	endpoints, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(endpoints))
	}
	ep := endpoints[0].(map[string]interface{})
	if ep["port"] != metricsPortName {
		t.Errorf("expected endpoint port %q, got %v", metricsPortName, ep["port"])
	}
	if ep["interval"] != "30s" {
		t.Errorf("expected interval 30s, got %v", ep["interval"])
	}

	// Owner reference must be set so it is GC'd with the DittoServer.
	if len(sm.GetOwnerReferences()) == 0 {
		t.Errorf("ServiceMonitor missing owner reference")
	}
}

// TestReconcileMetrics_ServiceMonitor_CRDAbsent_NoError is the critical
// anti-pattern guard: requesting a ServiceMonitor on a cluster without the
// prometheus-operator CRDs must NOT fail the reconcile.
func TestReconcileMetrics_ServiceMonitor_CRDAbsent_NoError(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", &dittoiov1alpha1.ServiceMonitorSpec{
		Enabled: true,
	})
	r := setupMetricsReconciler(t, false, ds)

	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics must not error when ServiceMonitor CRD is absent, got: %v", err)
	}

	// Metrics Service should still be created.
	if _, ok := getMetricsService(t, r, ds); !ok {
		t.Errorf("metrics Service should be created even without the ServiceMonitor CRD")
	}
	// ServiceMonitor must NOT be created.
	if _, ok := getServiceMonitor(t, r, ds); ok {
		t.Errorf("ServiceMonitor must not be created when the CRD is absent")
	}
}

// TestReconcileMetrics_ToggleOff_DeletesServiceMonitor verifies that disabling
// metrics on a LIVE CR removes both the Service and the ServiceMonitor (owner-ref
// GC only fires on CR deletion, not spec changes).
func TestReconcileMetrics_ToggleOff_DeletesServiceMonitor(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", &dittoiov1alpha1.ServiceMonitorSpec{Enabled: true})
	r := setupMetricsReconciler(t, true, ds)

	// Enable: create Service + ServiceMonitor.
	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics (enable) error: %v", err)
	}
	if _, ok := getServiceMonitor(t, r, ds); !ok {
		t.Fatalf("ServiceMonitor not created on enable")
	}

	// Toggle metrics off, reconcile again.
	ds.Spec.Metrics.Enabled = false
	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics (disable) error: %v", err)
	}
	if _, ok := getMetricsService(t, r, ds); ok {
		t.Errorf("metrics Service should be deleted after toggle-off")
	}
	if _, ok := getServiceMonitor(t, r, ds); ok {
		t.Errorf("ServiceMonitor should be deleted after toggle-off")
	}
}

// TestReconcileServiceMonitor_ToggleOff_DeletesMonitor verifies that turning off
// only the ServiceMonitor (metrics still enabled) removes the monitor but keeps
// the Service.
func TestReconcileServiceMonitor_ToggleOff_DeletesMonitor(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", &dittoiov1alpha1.ServiceMonitorSpec{Enabled: true})
	r := setupMetricsReconciler(t, true, ds)

	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics (enable) error: %v", err)
	}
	if _, ok := getServiceMonitor(t, r, ds); !ok {
		t.Fatalf("ServiceMonitor not created on enable")
	}

	ds.Spec.Metrics.ServiceMonitor.Enabled = false
	if err := r.reconcileMetrics(context.Background(), ds); err != nil {
		t.Fatalf("reconcileMetrics (SM off) error: %v", err)
	}
	if _, ok := getMetricsService(t, r, ds); !ok {
		t.Errorf("metrics Service should remain when only ServiceMonitor is disabled")
	}
	if _, ok := getServiceMonitor(t, r, ds); ok {
		t.Errorf("ServiceMonitor should be deleted when ServiceMonitor.Enabled=false")
	}
}

// TestBuildServiceMonitor_UserLabelsDoNotClobberInfra verifies user labels cannot
// overwrite the infra label keys the selector relies on.
func TestBuildServiceMonitor_UserLabelsDoNotClobberInfra(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", &dittoiov1alpha1.ServiceMonitorSpec{
		Enabled: true,
		Labels: map[string]string{
			"dittofs.io/metrics-service": "false", // attempt to clobber
			"release":                    "kps",
		},
	})
	r := setupMetricsReconciler(t, true, ds)
	sm := r.buildServiceMonitor(ds)

	labels := sm.GetLabels()
	if labels["dittofs.io/metrics-service"] != "true" {
		t.Errorf("infra label clobbered by user label: got %q", labels["dittofs.io/metrics-service"])
	}
	if labels["release"] != "kps" {
		t.Errorf("user label not preserved: got %q", labels["release"])
	}
}

func TestServiceMonitorCRDPresent_Gate(t *testing.T) {
	absent := setupMetricsReconciler(t, false)
	present := setupMetricsReconciler(t, true)

	got, err := absent.serviceMonitorCRDPresent()
	if err != nil {
		t.Fatalf("unexpected error (absent): %v", err)
	}
	if got {
		t.Errorf("expected CRD absent to report false")
	}

	got, err = present.serviceMonitorCRDPresent()
	if err != nil {
		t.Fatalf("unexpected error (present): %v", err)
	}
	if !got {
		t.Errorf("expected CRD present to report true")
	}
}

func TestBuildContainerPorts_MetricsPort(t *testing.T) {
	ds := metricsEnabledDittoServer("test-server", "default", nil)
	ports := buildContainerPorts(ds, nil)

	var found bool
	for _, p := range ports {
		if p.Name == metricsPortName {
			found = true
			if p.ContainerPort != 9090 {
				t.Errorf("expected metrics container port 9090, got %d", p.ContainerPort)
			}
		}
	}
	if !found {
		t.Errorf("metrics container port not present when metrics enabled")
	}

	// Disabled: no metrics port.
	dsDisabled := newTestDittoServer("test-server", "default")
	for _, p := range buildContainerPorts(dsDisabled, nil) {
		if p.Name == metricsPortName {
			t.Errorf("metrics container port should be absent when metrics disabled")
		}
	}
}

// sanity: the ServiceMonitor GVK constant is what we expect.
func TestServiceMonitorGVK(t *testing.T) {
	want := schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	if serviceMonitorGVK != want {
		t.Errorf("serviceMonitorGVK = %v, want %v", serviceMonitorGVK, want)
	}
}

// Avoid unused import if the metav1 helper is not otherwise referenced.
var _ = metav1.ObjectMeta{}
