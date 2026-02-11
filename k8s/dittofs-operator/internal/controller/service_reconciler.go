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
	"regexp"
	"sort"
	"strings"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/resources"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// adapterServiceLabel marks a Service as managed by the adapter service reconciler.
	adapterServiceLabel = "dittofs.io/adapter-service"
	// adapterTypeLabel stores the adapter type (e.g., "nfs", "smb") on a Service.
	adapterTypeLabel = "dittofs.io/adapter-type"
	// adapterPortPrefix is the naming prefix for dynamic adapter container ports.
	// This avoids collision with static port names like "nfs", "smb", "api", "metrics".
	adapterPortPrefix = "adapter-"
)

// invalidDNSChars matches characters not allowed in DNS-1035 labels.
var invalidDNSChars = regexp.MustCompile(`[^a-z0-9-]`)

// sanitizeAdapterType normalizes an adapter type string for use in K8s resource names,
// labels, and port names. Converts to lowercase, replaces invalid characters with hyphens,
// collapses consecutive hyphens, and trims leading/trailing hyphens.
func sanitizeAdapterType(adapterType string) string {
	s := strings.ToLower(adapterType)
	s = invalidDNSChars.ReplaceAllString(s, "-")
	// Collapse consecutive hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if s == "" {
		s = "unknown"
	}
	return s
}

// adapterPortName returns a K8s-safe container port name for an adapter.
// Port names must be IANA service names (max 15 chars, lowercase alphanumeric + hyphens).
func adapterPortName(adapterType string) string {
	name := adapterPortPrefix + sanitizeAdapterType(adapterType)
	if len(name) > 15 {
		name = name[:15]
	}
	name = strings.TrimRight(name, "-")
	return name
}

// adapterResourceName returns the canonical name for an adapter-owned K8s resource (Service, NetworkPolicy).
func adapterResourceName(crName, adapterType string) string {
	return fmt.Sprintf("%s-adapter-%s", crName, sanitizeAdapterType(adapterType))
}

// podSelectorLabels returns the standard labels used to select DittoFS server pods.
func podSelectorLabels(crName string) map[string]string {
	return map[string]string{
		"app":      "dittofs-server",
		"instance": crName,
	}
}

// adapterServiceLabels returns the labels for an adapter Service.
func adapterServiceLabels(crName, adapterType string) map[string]string {
	labels := podSelectorLabels(crName)
	labels[adapterServiceLabel] = "true"
	labels[adapterTypeLabel] = sanitizeAdapterType(adapterType)
	return labels
}

// getAdapterServiceType reads the Service type from the CRD spec.adapterServices.type,
// defaulting to LoadBalancer if not set.
func getAdapterServiceType(ds *dittoiov1alpha1.DittoServer) corev1.ServiceType {
	if ds.Spec.AdapterServices != nil && ds.Spec.AdapterServices.Type != "" {
		return corev1.ServiceType(ds.Spec.AdapterServices.Type)
	}
	return corev1.ServiceTypeLoadBalancer
}

// getAdapterServiceAnnotations reads annotations from the CRD spec.adapterServices.annotations.
// Returns nil if not set.
func getAdapterServiceAnnotations(ds *dittoiov1alpha1.DittoServer) map[string]string {
	if ds.Spec.AdapterServices != nil {
		return ds.Spec.AdapterServices.Annotations
	}
	return nil
}

// reconcileAdapterServices synchronizes K8s Services with the discovered adapter state.
// It creates Services for enabled+running adapters, updates Services when ports change,
// and deletes Services for stopped/removed adapters. Only manages Services with the
// dittofs.io/adapter-service label -- never touches static Services.
func (r *DittoServerReconciler) reconcileAdapterServices(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	logger := logf.FromContext(ctx)

	// DISC-03 safety: if no successful poll has occurred yet, skip entirely.
	adapters := r.getLastKnownAdapters(ds)
	if adapters == nil {
		logger.V(1).Info("No adapter poll yet, skipping adapter service reconciliation")
		return nil
	}

	desired := activeAdaptersByType(adapters)

	// List existing adapter Services using label selector.
	var existingList corev1.ServiceList
	if err := r.List(ctx, &existingList,
		client.InNamespace(ds.Namespace),
		client.MatchingLabels{
			adapterServiceLabel: "true",
			"instance":          ds.Name,
		},
	); err != nil {
		return fmt.Errorf("failed to list adapter services: %w", err)
	}

	// Build actual set keyed by adapter type.
	actual := make(map[string]*corev1.Service)
	for i := range existingList.Items {
		svc := &existingList.Items[i]
		adapterType := svc.Labels[adapterTypeLabel]
		if adapterType != "" {
			actual[adapterType] = svc
		}
	}

	// Create Services for desired adapters not yet present.
	for adapterType, info := range desired {
		if _, exists := actual[adapterType]; !exists {
			if err := r.createAdapterService(ctx, ds, adapterType, info); err != nil {
				return fmt.Errorf("failed to create adapter service for %s: %w", adapterType, err)
			}
		}
	}

	// Update Services that exist and are still desired (port change detection).
	for adapterType, svc := range actual {
		if info, stillDesired := desired[adapterType]; stillDesired {
			if err := r.updateAdapterServiceIfNeeded(ctx, ds, svc, info); err != nil {
				return fmt.Errorf("failed to update adapter service for %s: %w", adapterType, err)
			}
		}
	}

	// Delete Services for adapters that are no longer desired.
	for adapterType, svc := range actual {
		if _, stillDesired := desired[adapterType]; !stillDesired {
			if err := r.deleteAdapterService(ctx, ds, svc, adapterType); err != nil {
				return fmt.Errorf("failed to delete adapter service for %s: %w", adapterType, err)
			}
		}
	}

	// Reconcile StatefulSet container ports to match active adapters.
	return r.reconcileContainerPorts(ctx, ds, desired)
}

// createAdapterService creates a new K8s Service for an adapter.
func (r *DittoServerReconciler) createAdapterService(ctx context.Context, ds *dittoiov1alpha1.DittoServer, adapterType string, info AdapterInfo) error {
	svcName := adapterResourceName(ds.Name, adapterType)
	labels := adapterServiceLabels(ds.Name, adapterType)
	svcType := getAdapterServiceType(ds)
	annotations := getAdapterServiceAnnotations(ds)

	sanitizedType := sanitizeAdapterType(adapterType)
	builder := resources.NewServiceBuilder(svcName, ds.Namespace).
		WithLabels(labels).
		WithSelector(podSelectorLabels(ds.Name)).
		WithType(svcType).
		AddTCPPort(sanitizedType, int32(info.Port))

	if annotations != nil {
		builder.WithAnnotations(annotations)
	}

	svc := builder.Build()

	// Set owner reference for garbage collection.
	if err := controllerutil.SetControllerReference(ds, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	if err := r.Create(ctx, svc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Fall through to update path on next reconcile.
			return nil
		}
		return fmt.Errorf("failed to create service %s: %w", svcName, err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceCreated",
		"Created %s Service %s for adapter %s (port %d)", svcType, svc.Name, adapterType, info.Port)

	return nil
}

// updateAdapterServiceIfNeeded updates an existing adapter Service if its port, type, or annotations changed.
func (r *DittoServerReconciler) updateAdapterServiceIfNeeded(ctx context.Context, ds *dittoiov1alpha1.DittoServer, existing *corev1.Service, info AdapterInfo) error {
	desiredPort := int32(info.Port)
	desiredType := getAdapterServiceType(ds)
	desiredAnnotations := getAdapterServiceAnnotations(ds)

	// Treat empty/malformed ports as drift so the Service self-heals.
	portChanged := len(existing.Spec.Ports) == 0 || existing.Spec.Ports[0].Port != desiredPort
	typeChanged := existing.Spec.Type != desiredType
	annotationsChanged := !annotationsMatch(existing.Annotations, desiredAnnotations)

	if !portChanged && !typeChanged && !annotationsChanged {
		return nil
	}

	// Re-fetch fresh copy for optimistic locking.
	fresh := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(existing), fresh); err != nil {
		return fmt.Errorf("failed to get fresh service: %w", err)
	}

	oldPort := int32(0)
	if len(fresh.Spec.Ports) > 0 {
		oldPort = fresh.Spec.Ports[0].Port
	}

	// Rebuild port to the desired single-port shape, ensuring TargetPort is always correct.
	adapterType := fresh.Labels[adapterTypeLabel]
	fresh.Spec.Ports = []corev1.ServicePort{
		{
			Name:       adapterType,
			Port:       desiredPort,
			TargetPort: intstr.FromInt32(desiredPort),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	// Update Service type.
	fresh.Spec.Type = desiredType

	// Update annotations.
	if desiredAnnotations != nil {
		if fresh.Annotations == nil {
			fresh.Annotations = make(map[string]string)
		}
		for k, v := range desiredAnnotations {
			fresh.Annotations[k] = v
		}
	}

	err := retryOnConflict(func() error {
		return r.Update(ctx, fresh)
	})
	if err != nil {
		return fmt.Errorf("failed to update service: %w", err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceUpdated",
		"Updated Service %s for adapter %s (port %d -> %d)", fresh.Name, adapterType, oldPort, desiredPort)

	return nil
}

// deleteAdapterService deletes a K8s Service for a stopped/removed adapter.
func (r *DittoServerReconciler) deleteAdapterService(ctx context.Context, ds *dittoiov1alpha1.DittoServer, svc *corev1.Service, adapterType string) error {
	if err := r.Delete(ctx, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete service %s: %w", svc.Name, err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceDeleted",
		"Deleted Service %s for adapter %s", svc.Name, adapterType)

	return nil
}

// annotationsMatch checks if existing annotations contain all desired annotations with correct values.
func annotationsMatch(existing, desired map[string]string) bool {
	if len(desired) == 0 {
		return true
	}
	for k, v := range desired {
		if existing[k] != v {
			return false
		}
	}
	return true
}

// reconcileContainerPorts updates the StatefulSet container ports to include dynamic adapter
// ports for active adapters. Static ports (those not prefixed with "adapter-") are preserved
// unchanged. The StatefulSet is only updated when container ports actually change, avoiding
// unnecessary rolling restarts.
func (r *DittoServerReconciler) reconcileContainerPorts(ctx context.Context, ds *dittoiov1alpha1.DittoServer, activeAdapters map[string]AdapterInfo) error {
	logger := logf.FromContext(ctx)

	// Get the StatefulSet.
	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: ds.Name}, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet may not exist yet on first reconcile.
			logger.V(1).Info("StatefulSet not found, skipping container port reconciliation")
			return nil
		}
		return fmt.Errorf("failed to get StatefulSet for port reconciliation: %w", err)
	}

	// Ensure there is at least one container.
	if len(statefulSet.Spec.Template.Spec.Containers) == 0 {
		logger.V(1).Info("StatefulSet has no containers, skipping container port reconciliation")
		return nil
	}

	// Get current container ports.
	currentPorts := statefulSet.Spec.Template.Spec.Containers[0].Ports

	// Separate current ports into static (no adapter- prefix) and dynamic (adapter- prefix).
	var staticPorts []corev1.ContainerPort
	for _, p := range currentPorts {
		if !strings.HasPrefix(p.Name, adapterPortPrefix) {
			staticPorts = append(staticPorts, p)
		}
	}

	// Build desired dynamic adapter ports.
	var dynamicPorts []corev1.ContainerPort
	for adapterType, info := range activeAdapters {
		dynamicPorts = append(dynamicPorts, corev1.ContainerPort{
			Name:          adapterPortName(adapterType),
			ContainerPort: int32(info.Port),
			Protocol:      corev1.ProtocolTCP,
		})
	}

	// Build final desired ports: static (preserved) + dynamic (from active adapters).
	desiredPorts := make([]corev1.ContainerPort, 0, len(staticPorts)+len(dynamicPorts))
	desiredPorts = append(desiredPorts, staticPorts...)
	desiredPorts = append(desiredPorts, dynamicPorts...)

	// Sort both for deterministic comparison.
	sortContainerPorts(currentPorts)
	sortContainerPorts(desiredPorts)

	// Compare: if ports are equal, no update needed (avoids unnecessary rolling restarts).
	if portsEqual(currentPorts, desiredPorts) {
		logger.V(1).Info("Container ports unchanged, skipping StatefulSet update")
		return nil
	}

	// Re-fetch fresh StatefulSet for optimistic locking.
	fresh := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: ds.Name}, fresh); err != nil {
		return fmt.Errorf("failed to re-fetch StatefulSet: %w", err)
	}

	// Update container ports (only PodTemplateSpec, never VolumeClaimTemplates).
	fresh.Spec.Template.Spec.Containers[0].Ports = desiredPorts

	err := retryOnConflict(func() error {
		return r.Update(ctx, fresh)
	})
	if err != nil {
		return fmt.Errorf("failed to update StatefulSet container ports: %w", err)
	}

	logger.Info("Updated StatefulSet container ports", "ports", len(desiredPorts))
	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "ContainerPortsUpdated",
		"Updated StatefulSet container ports (%d static + %d dynamic)", len(staticPorts), len(dynamicPorts))

	return nil
}

// sortContainerPorts sorts a slice of container ports by Name for deterministic comparison.
func sortContainerPorts(ports []corev1.ContainerPort) {
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Name < ports[j].Name
	})
}

// portsEqual returns true if two sorted container port slices are identical.
func portsEqual(a, b []corev1.ContainerPort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].ContainerPort != b[i].ContainerPort || a[i].Protocol != b[i].Protocol {
			return false
		}
	}
	return true
}
