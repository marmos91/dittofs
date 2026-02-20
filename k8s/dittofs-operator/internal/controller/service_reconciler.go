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
	"crypto/sha256"
	"encoding/hex"
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

	// portmapperServicePort is the standard RFC 1057 portmapper port that NFS clients query.
	portmapperServicePort = int32(111)
	// portmapperContainerPort is the unprivileged container port the portmapper listens on.
	// K8s Service port mapping (111 → 10111) avoids needing privileged security context.
	portmapperContainerPort = int32(10111)
	// portmapperPortName is the Service/container port name for the portmapper.
	portmapperPortName = "portmap"
	// portmapperContainerPortName is the container port name for the portmapper.
	// Uses the adapter- prefix so it's managed as a dynamic port and cleaned up with NFS.
	portmapperContainerPortName = "adapter-portmap"
	// nfsAdapterType is the canonical sanitized type string for the NFS adapter.
	nfsAdapterType = "nfs"
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
// When truncation is needed, a 4-char hash suffix is appended to prevent collisions
// between adapter types that share the same prefix (e.g., "custom-adapter-a" vs "custom-adapter-b").
func adapterPortName(adapterType string) string {
	sanitized := sanitizeAdapterType(adapterType)
	name := adapterPortPrefix + sanitized
	if len(name) > 15 {
		// Use hash suffix for disambiguation: "adapter-xx-abcd" (15 chars)
		hash := sha256.Sum256([]byte(sanitized))
		suffix := hex.EncodeToString(hash[:2]) // 4 hex chars
		// adapterPortPrefix is 8 chars, dash is 1, suffix is 4 = 13, leaving 2 for type prefix
		prefix := name[:10] // "adapter-" (8) + 2 chars of type
		prefix = strings.TrimRight(prefix, "-")
		name = prefix + "-" + suffix
	}
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

// isNFSAdapter returns true if the sanitized adapter type is "nfs".
func isNFSAdapter(adapterType string) bool {
	return sanitizeAdapterType(adapterType) == nfsAdapterType
}

// buildAdapterServicePorts returns the Service ports for an adapter.
// NFS adapters get 2 ports (NFS + portmapper 111→10111), all others get 1.
func buildAdapterServicePorts(adapterType string, info AdapterInfo) []corev1.ServicePort {
	portName := adapterPortName(adapterType)
	ports := []corev1.ServicePort{
		{
			Name:       portName,
			Port:       int32(info.Port),
			TargetPort: intstr.FromInt32(int32(info.Port)),
			Protocol:   corev1.ProtocolTCP,
		},
	}
	if isNFSAdapter(adapterType) {
		ports = append(ports, corev1.ServicePort{
			Name:       portmapperPortName,
			Port:       portmapperServicePort,
			TargetPort: intstr.FromInt32(portmapperContainerPort),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}

// servicePortsMatch returns true if two Service port slices are equivalent,
// comparing by name, port, targetPort, and protocol. NodePort is ignored because
// K8s auto-assigns it.
func servicePortsMatch(existing, desired []corev1.ServicePort) bool {
	if len(existing) != len(desired) {
		return false
	}
	// Build map for O(n) comparison instead of requiring sorted input.
	type portKey struct {
		name       string
		port       int32
		targetPort int32
		protocol   corev1.Protocol
	}
	set := make(map[portKey]struct{}, len(desired))
	for _, p := range desired {
		set[portKey{p.Name, p.Port, p.TargetPort.IntVal, p.Protocol}] = struct{}{}
	}
	for _, p := range existing {
		if _, ok := set[portKey{p.Name, p.Port, p.TargetPort.IntVal, p.Protocol}]; !ok {
			return false
		}
	}
	return true
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

	builder := resources.NewServiceBuilder(svcName, ds.Namespace).
		WithLabels(labels).
		WithSelector(podSelectorLabels(ds.Name)).
		WithType(svcType)

	for _, sp := range buildAdapterServicePorts(adapterType, info) {
		builder.AddTCPPortWithTarget(sp.Name, sp.Port, sp.TargetPort.IntVal)
	}

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

// updateAdapterServiceIfNeeded updates an existing adapter Service if its ports, type, or annotations changed.
func (r *DittoServerReconciler) updateAdapterServiceIfNeeded(ctx context.Context, ds *dittoiov1alpha1.DittoServer, existing *corev1.Service, info AdapterInfo) error {
	adapterType := existing.Labels[adapterTypeLabel]
	desiredPorts := buildAdapterServicePorts(adapterType, info)
	desiredType := getAdapterServiceType(ds)
	desiredAnnotations := getAdapterServiceAnnotations(ds)

	portsChanged := !servicePortsMatch(existing.Spec.Ports, desiredPorts)
	typeChanged := existing.Spec.Type != desiredType
	annotationsChanged := !annotationsMatch(existing.Annotations, desiredAnnotations)

	if !portsChanged && !typeChanged && !annotationsChanged {
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

	// Rebuild ports to desired shape (single port for most adapters, multi-port for NFS).
	fresh.Spec.Ports = desiredPorts

	// Update Service type.
	fresh.Spec.Type = desiredType

	// Update annotations (add desired, preserving third-party annotations).
	fresh.Annotations = syncManagedAnnotations(fresh.Annotations, desiredAnnotations)

	if err := r.Update(ctx, fresh); err != nil {
		return fmt.Errorf("failed to update service: %w", err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceUpdated",
		"Updated Service %s for adapter %s (port %d -> %d)", fresh.Name, adapterType, oldPort, int32(info.Port))

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

// managedAnnotationsKey is the annotation that tracks which annotation keys the operator manages.
// This enables cleanup when annotations are removed from the CRD spec.
const managedAnnotationsKey = "dittofs.io/managed-annotations"

// annotationsMatch checks if existing annotations contain all desired annotations with correct values
// and that no stale operator-managed annotations remain from a previous spec.
func annotationsMatch(existing, desired map[string]string) bool {
	for k, v := range desired {
		if existing[k] != v {
			return false
		}
	}
	// Check for stale managed annotations that should be removed.
	if managed, ok := existing[managedAnnotationsKey]; ok && managed != "" {
		for _, k := range strings.Split(managed, ",") {
			if _, stillDesired := desired[k]; !stillDesired {
				return false
			}
		}
	}
	return true
}

// syncManagedAnnotations applies desired annotations and removes stale ones that were
// previously set by the operator. Tracks managed keys in a metadata annotation so
// third-party annotations are never removed.
func syncManagedAnnotations(annotations map[string]string, desired map[string]string) map[string]string {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	// Remove previously managed keys that are no longer desired.
	if managed, ok := annotations[managedAnnotationsKey]; ok && managed != "" {
		for _, k := range strings.Split(managed, ",") {
			if _, stillDesired := desired[k]; !stillDesired {
				delete(annotations, k)
			}
		}
	}
	// Apply desired annotations.
	for k, v := range desired {
		annotations[k] = v
	}
	// Update the tracking annotation.
	if len(desired) > 0 {
		keys := make([]string, 0, len(desired))
		for k := range desired {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		annotations[managedAnnotationsKey] = strings.Join(keys, ",")
	} else {
		delete(annotations, managedAnnotationsKey)
	}
	return annotations
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
		// NFS adapter also needs the portmapper container port.
		if isNFSAdapter(adapterType) {
			dynamicPorts = append(dynamicPorts, corev1.ContainerPort{
				Name:          portmapperContainerPortName,
				ContainerPort: portmapperContainerPort,
				Protocol:      corev1.ProtocolTCP,
			})
		}
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

	if err := r.Update(ctx, fresh); err != nil {
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
