package controller

import (
	"context"
	"fmt"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/resources"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// metricsPortName is the named container/Service port for the metrics endpoint.
	metricsPortName = "metrics"
	// metricsServiceSuffix is appended to the CR name for the metrics Service.
	metricsServiceSuffix = "-metrics"
	// metricsTokenVolumeName is the pod volume that projects the scrape bearer token.
	metricsTokenVolumeName = "metrics-token"

	// Prometheus annotation-based discovery keys (for clusters not running the
	// prometheus-operator). Honored by the classic kubernetes_sd scrape config.
	prometheusScrapeAnnotation = "prometheus.io/scrape"
	prometheusPortAnnotation   = "prometheus.io/port"
	prometheusPathAnnotation   = "prometheus.io/path"
)

// serviceMonitorGVK is the GroupVersionKind of the prometheus-operator
// ServiceMonitor CRD. We address it via unstructured so the operator does not
// take a compile-time dependency on the prometheus-operator Go module.
var serviceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "ServiceMonitor",
}

// reconcileMetrics reconciles the metrics integration objects (metrics Service
// and, when present + enabled, the prometheus-operator ServiceMonitor). It is a
// no-op when metrics are disabled. Failures are surfaced to the caller, except
// the ServiceMonitor CRD-absence case which is deliberately swallowed (see
// reconcileServiceMonitor) so a cluster without the prometheus-operator never
// wedges the reconcile.
func (r *DittoServerReconciler) reconcileMetrics(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	if !ds.MetricsEnabled() {
		// Best-effort cleanup when metrics are toggled off on a LIVE CR: owner-ref
		// GC only fires on DittoServer deletion, not on spec changes, so an
		// orphaned Service/ServiceMonitor would otherwise keep a stale Prometheus
		// target. Drop the ServiceMonitor first (CRD-gated), then the Service.
		if err := r.deleteServiceMonitor(ctx, ds); err != nil {
			return err
		}
		return r.deleteMetricsService(ctx, ds)
	}

	if err := r.reconcileMetricsService(ctx, ds); err != nil {
		return fmt.Errorf("failed to reconcile metrics Service: %w", err)
	}

	return r.reconcileServiceMonitor(ctx, ds)
}

// metricsServiceName returns the canonical name of the metrics Service.
func metricsServiceName(crName string) string {
	return crName + metricsServiceSuffix
}

// reconcileMetricsService creates/updates a dedicated ClusterIP Service for the
// metrics endpoint. It carries prometheus.io/* annotations so annotation-based
// scrape discovery works without the prometheus-operator, and is the selector
// target for the ServiceMonitor.
func (r *DittoServerReconciler) reconcileMetricsService(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	labels := metricsServiceLabels(ds.Name)

	annotations := map[string]string{
		prometheusScrapeAnnotation: "true",
		prometheusPortAnnotation:   fmt.Sprintf("%d", ds.MetricsPort()),
		prometheusPathAnnotation:   ds.MetricsPath(),
	}

	svc := resources.NewServiceBuilder(metricsServiceName(ds.Name), ds.Namespace).
		WithLabels(labels).
		WithSelector(podSelectorLabels(ds.Name)).
		WithAnnotations(annotations).
		AddTCPPort(metricsPortName, ds.MetricsPort()).
		Build()

	return r.createOrUpdateService(ctx, ds, svc)
}

// metricsServiceLabels returns labels for the metrics Service. They double as
// the ServiceMonitor's selector matchLabels so the monitor targets exactly this
// Service.
func metricsServiceLabels(crName string) map[string]string {
	labels := podSelectorLabels(crName)
	labels["dittofs.io/metrics-service"] = "true"
	return labels
}

// deleteMetricsService removes the metrics Service if present. Not-found is
// ignored. Used to clean up when metrics are toggled off on a live CR.
func (r *DittoServerReconciler) deleteMetricsService(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metricsServiceName(ds.Name),
			Namespace: ds.Namespace,
		},
	}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete metrics Service: %w", err)
	}
	return nil
}

// deleteServiceMonitor removes the ServiceMonitor we manage, if present. It is
// CRD-gated: on a cluster without the prometheus-operator the GVK is unknown, so
// there is nothing to delete and the absent-CRD case must not error. Not-found
// is ignored.
func (r *DittoServerReconciler) deleteServiceMonitor(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	present, err := r.serviceMonitorCRDPresent()
	if err != nil {
		return fmt.Errorf("failed to check ServiceMonitor CRD presence: %w", err)
	}
	if !present {
		return nil
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(metricsServiceName(ds.Name))
	sm.SetNamespace(ds.Namespace)
	if err := r.Delete(ctx, sm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete ServiceMonitor: %w", err)
	}
	return nil
}

// serviceMonitorCRDPresent reports whether the monitoring.coreos.com/v1
// ServiceMonitor CRD is installed in the cluster. It mirrors the PerconaPGCluster
// discovery gate in SetupWithManager: a NoMatch (CRD absent) is not an error,
// it just means the integration is unavailable.
func (r *DittoServerReconciler) serviceMonitorCRDPresent() (bool, error) {
	_, err := r.RESTMapper().RESTMapping(serviceMonitorGVK.GroupKind(), serviceMonitorGVK.Version)
	if err != nil {
		if apimeta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// reconcileServiceMonitor creates/updates a prometheus-operator ServiceMonitor
// for the metrics Service, but ONLY when (a) the user opted in via
// spec.metrics.serviceMonitor.enabled AND (b) the monitoring.coreos.com CRDs are
// present. When the CRDs are absent it logs at Info and returns nil — it never
// fails the reconcile (the MinIO-operator hard-fail anti-pattern we avoid).
//
// The object is built as unstructured so the operator carries no compile-time
// dependency on the prometheus-operator module.
func (r *DittoServerReconciler) reconcileServiceMonitor(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	logger := logf.FromContext(ctx)

	if !ds.ServiceMonitorEnabled() {
		// ServiceMonitor was turned off (but metrics stay on): remove any monitor
		// we previously created so it does not keep scraping. CRD-gated so this is
		// a no-op on clusters without the prometheus-operator.
		return r.deleteServiceMonitor(ctx, ds)
	}

	present, err := r.serviceMonitorCRDPresent()
	if err != nil {
		return fmt.Errorf("failed to check ServiceMonitor CRD presence: %w", err)
	}
	if !present {
		logger.Info("ServiceMonitor requested but monitoring.coreos.com CRDs are not installed; " +
			"skipping ServiceMonitor creation (install the prometheus-operator to enable it)")
		return nil
	}

	desired := r.buildServiceMonitor(ds)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(serviceMonitorGVK)
	existing.SetName(desired.GetName())
	existing.SetNamespace(desired.GetNamespace())

	return retryOnConflict(func() error {
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
			// Owner reference so the ServiceMonitor is GC'd with the DittoServer.
			if err := controllerutil.SetControllerReference(ds, existing, r.Scheme); err != nil {
				return err
			}
			existing.SetLabels(desired.GetLabels())
			existing.Object["spec"] = desired.Object["spec"]
			return nil
		})
		if err != nil {
			return err
		}
		logger.V(1).Info("Reconciled ServiceMonitor", "operation", op, "name", desired.GetName())
		return nil
	})
}

// buildServiceMonitor constructs the desired ServiceMonitor as unstructured. The
// endpoint targets the metrics Service's named port, with the configured path
// and optional scrape interval; when a bearer token Secret is referenced the
// endpoint presents it so authed scraping works.
func (r *DittoServerReconciler) buildServiceMonitor(ds *dittoiov1alpha1.DittoServer) *unstructured.Unstructured {
	// The selector must match the metrics Service's own labels exactly. User-
	// supplied labels live on the ServiceMonitor itself (so a Prometheus
	// serviceMonitorSelector can match it, e.g. {release: kube-prometheus-stack}).
	// Apply user labels FIRST, then overlay the infra labels so a user label
	// cannot clobber the keys the selector relies on (e.g. dittofs.io/metrics-service).
	serviceLabels := metricsServiceLabels(ds.Name)
	monitorLabels := make(map[string]string)

	endpoint := map[string]interface{}{
		"port": metricsPortName,
		"path": ds.MetricsPath(),
	}
	if sm := ds.Spec.Metrics.ServiceMonitor; sm != nil {
		for k, v := range sm.Labels {
			monitorLabels[k] = v
		}
		if sm.Interval != "" {
			endpoint["interval"] = sm.Interval
		}
	}
	for k, v := range serviceLabels {
		monitorLabels[k] = v
	}
	if ref := ds.MetricsBearerTokenSecret(); ref != nil {
		endpoint["bearerTokenSecret"] = map[string]interface{}{
			"name": ref.Name,
			"key":  ref.Key,
		}
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"matchLabels": toStringMap(serviceLabels),
				},
				"namespaceSelector": map[string]interface{}{
					"matchNames": []interface{}{ds.Namespace},
				},
				"endpoints": []interface{}{endpoint},
			},
		},
	}
	obj.SetGroupVersionKind(serviceMonitorGVK)
	obj.SetName(metricsServiceName(ds.Name))
	obj.SetNamespace(ds.Namespace)
	obj.SetLabels(monitorLabels)
	return obj
}

// toStringMap converts a map[string]string to the map[string]interface{} shape
// unstructured nested fields require.
func toStringMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
