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
	"slices"
	"strings"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const labelTrue = "true"

const (
	// adapterNetworkPolicyLabel marks a NetworkPolicy as managed by the adapter NetworkPolicy reconciler.
	adapterNetworkPolicyLabel = "dittofs.io/adapter-networkpolicy"

	// baselineNetworkPolicyLabel marks the baseline NetworkPolicy that allows API/health traffic.
	baselineNetworkPolicyLabel = "dittofs.io/baseline-networkpolicy"
)

// networkPolicyLabels returns the labels for an adapter NetworkPolicy.
func networkPolicyLabels(crName, adapterType string) map[string]string {
	labels := podSelectorLabels(crName)
	labels[adapterNetworkPolicyLabel] = labelTrue
	labels[adapterTypeLabel] = sanitizeAdapterType(adapterType)
	return labels
}

// formatIngressPorts formats NetworkPolicy ingress ports for event messages.
func formatIngressPorts(ports []networkingv1.NetworkPolicyPort) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.Port != nil {
			proto := corev1.ProtocolTCP
			if p.Protocol != nil {
				proto = *p.Protocol
			}
			parts = append(parts, fmt.Sprintf("%d/%s", p.Port.IntVal, proto))
		}
	}
	return fmt.Sprintf("ports %s", strings.Join(parts, ", "))
}

// buildAdapterIngressPorts returns the NetworkPolicy ingress ports for an adapter.
// NFS adapters get 3 ports (NFS + portmapper TCP/UDP), all others get 1.
func buildAdapterIngressPorts(adapterType string, port int32) []networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	ports := []networkingv1.NetworkPolicyPort{
		{
			Protocol: &tcp,
			Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: port},
		},
	}
	if isNFSAdapter(adapterType) {
		udp := corev1.ProtocolUDP
		ports = append(ports,
			networkingv1.NetworkPolicyPort{
				Protocol: &tcp,
				Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: portmapperContainerPort},
			},
			networkingv1.NetworkPolicyPort{
				Protocol: &udp,
				Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: portmapperContainerPort},
			},
		)
	}
	return ports
}

// buildAdapterNetworkPolicy constructs a NetworkPolicy allowing ingress on adapter port(s).
// NFS adapters get the NFS port (TCP) and the portmapper container port (TCP + UDP).
// Only ingress is restricted; egress is left unrestricted because DittoFS pods need outbound
// access to S3, external metadata stores, and other backend services.
func buildAdapterNetworkPolicy(crName, namespace, adapterType string, port int32) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adapterResourceName(crName, adapterType),
			Namespace: namespace,
			Labels:    networkPolicyLabels(crName, adapterType),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: podSelectorLabels(crName),
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: buildAdapterIngressPorts(adapterType, port),
				},
			},
		},
	}
}

// baselineNetworkPolicyName returns the name for the baseline NetworkPolicy.
func baselineNetworkPolicyName(crName string) string {
	return crName + "-baseline"
}

// baselineIngressPorts returns the ingress ports for the baseline NetworkPolicy.
// Always includes the API port.
func baselineIngressPorts(ds *dittoiov1alpha1.DittoServer) []networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	ports := []networkingv1.NetworkPolicyPort{
		{
			Protocol: &tcp,
			Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: getAPIPort(ds)},
		},
	}
	return ports
}

// ensureBaselineNetworkPolicy ensures a NetworkPolicy exists that allows API traffic.
// This prevents adapter NetworkPolicies from blocking operator-to-API communication.
//
// This is called from the main Reconcile loop BEFORE auth, and also from reconcileNetworkPolicies.
// The early call is intentional: the baseline NP must exist before adapter NPs are created,
// otherwise the first adapter NP would activate default-deny and block API traffic (chicken-and-egg).
func (r *DittoServerReconciler) ensureBaselineNetworkPolicy(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	npName := baselineNetworkPolicyName(ds.Name)
	desiredLabels := map[string]string{
		"app":                      "dittofs-server",
		"instance":                 ds.Name,
		baselineNetworkPolicyLabel: labelTrue,
	}
	desiredIngress := baselineIngressPorts(ds)

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npName,
			Namespace: ds.Namespace,
			Labels:    desiredLabels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: podSelectorLabels(ds.Name),
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: desiredIngress},
			},
		},
	}

	if err := controllerutil.SetControllerReference(ds, np, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference on baseline network policy: %w", err)
	}

	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: npName}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, np); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create baseline network policy: %w", err)
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get baseline network policy: %w", err)
	}

	// Converge metadata (labels, ownerReferences) and spec to desired state.
	needsUpdate := false
	for k, v := range desiredLabels {
		if existing.Labels[k] != v {
			needsUpdate = true
			break
		}
	}
	if !ingressPortsMatch(existing.Spec.Ingress, desiredIngress) {
		needsUpdate = true
	}
	if !hasOwnerReference(existing, ds) {
		needsUpdate = true
	}

	if needsUpdate {
		existing.Labels = desiredLabels
		existing.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{
			{Ports: desiredIngress},
		}
		if err := controllerutil.SetControllerReference(ds, existing, r.Scheme); err != nil {
			return fmt.Errorf("failed to set controller reference on baseline network policy: %w", err)
		}
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update baseline network policy: %w", err)
		}
	}

	return nil
}

// ingressPortsMatch checks if existing ingress rules match the desired ports.
func ingressPortsMatch(existing []networkingv1.NetworkPolicyIngressRule, desired []networkingv1.NetworkPolicyPort) bool {
	if len(existing) == 0 {
		return len(desired) == 0
	}
	existingPorts := existing[0].Ports
	if len(existingPorts) != len(desired) {
		return false
	}
	for i := range desired {
		if existingPorts[i].Port == nil || desired[i].Port == nil {
			return false
		}
		if existingPorts[i].Port.IntVal != desired[i].Port.IntVal {
			return false
		}
		// Compare protocols (nil defaults to TCP).
		ep := corev1.ProtocolTCP
		dp := corev1.ProtocolTCP
		if existingPorts[i].Protocol != nil {
			ep = *existingPorts[i].Protocol
		}
		if desired[i].Protocol != nil {
			dp = *desired[i].Protocol
		}
		if ep != dp {
			return false
		}
	}
	return true
}

// hasOwnerReference checks if the object has an owner reference to the given owner.
func hasOwnerReference(obj, owner client.Object) bool {
	return slices.ContainsFunc(obj.GetOwnerReferences(), func(ref metav1.OwnerReference) bool {
		return ref.UID == owner.GetUID()
	})
}

// reconcileNetworkPolicies synchronizes K8s NetworkPolicies with the discovered adapter state.
// It creates NetworkPolicies for enabled+running adapters, updates when ports change,
// and deletes NetworkPolicies for stopped/removed adapters. Only manages NetworkPolicies
// with the dittofs.io/adapter-networkpolicy label -- never touches static NetworkPolicies.
// Unlike adapter Services, NetworkPolicy errors are propagated (security-critical).
func (r *DittoServerReconciler) reconcileNetworkPolicies(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	logger := logf.FromContext(ctx)

	// Ensure baseline NetworkPolicy is converged (idempotent, also called from main Reconcile).
	// Must run before adapter NPs: creating adapter NPs without a baseline would activate
	// default-deny and block the API port (chicken-and-egg problem).
	if err := r.ensureBaselineNetworkPolicy(ctx, ds); err != nil {
		return fmt.Errorf("failed to ensure baseline network policy: %w", err)
	}

	// DISC-03 safety: if no successful poll has occurred yet, skip adapter NP reconciliation.
	adapters := r.getLastKnownAdapters(ds)
	if adapters == nil {
		logger.V(1).Info("No adapter poll yet, skipping adapter NetworkPolicy reconciliation")
		return nil
	}

	desired := activeAdaptersByType(adapters)

	// List existing adapter NetworkPolicies using label selector.
	var existingList networkingv1.NetworkPolicyList
	if err := r.List(ctx, &existingList,
		client.InNamespace(ds.Namespace),
		client.MatchingLabels{
			adapterNetworkPolicyLabel: labelTrue,
			"instance":                ds.Name,
		},
	); err != nil {
		return fmt.Errorf("failed to list adapter network policies: %w", err)
	}

	// Build actual set keyed by adapter type.
	actual := make(map[string]*networkingv1.NetworkPolicy)
	for i := range existingList.Items {
		np := &existingList.Items[i]
		adapterType := np.Labels[adapterTypeLabel]
		if adapterType != "" {
			actual[adapterType] = np
		}
	}

	// Create NetworkPolicies for desired adapters not yet present.
	for adapterType, info := range desired {
		if _, exists := actual[adapterType]; !exists {
			if err := r.createAdapterNetworkPolicy(ctx, ds, adapterType, info); err != nil {
				return fmt.Errorf("failed to create adapter network policy for %s: %w", adapterType, err)
			}
		}
	}

	// Update NetworkPolicies that exist and are still desired (port change detection).
	for adapterType, np := range actual {
		if info, stillDesired := desired[adapterType]; stillDesired {
			if err := r.updateAdapterNetworkPolicyIfNeeded(ctx, ds, np, info); err != nil {
				return fmt.Errorf("failed to update adapter network policy for %s: %w", adapterType, err)
			}
		}
	}

	// Delete NetworkPolicies for adapters that are no longer desired.
	for adapterType, np := range actual {
		if _, stillDesired := desired[adapterType]; !stillDesired {
			if err := r.deleteAdapterNetworkPolicy(ctx, ds, np, adapterType); err != nil {
				return fmt.Errorf("failed to delete adapter network policy for %s: %w", adapterType, err)
			}
		}
	}

	return nil
}

// createAdapterNetworkPolicy creates a new K8s NetworkPolicy for an adapter.
func (r *DittoServerReconciler) createAdapterNetworkPolicy(ctx context.Context, ds *dittoiov1alpha1.DittoServer, adapterType string, info AdapterInfo) error {
	np := buildAdapterNetworkPolicy(ds.Name, ds.Namespace, adapterType, int32(info.Port))

	// Set owner reference for garbage collection.
	if err := controllerutil.SetControllerReference(ds, np, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	if err := r.Create(ctx, np); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Fall through to update path on next reconcile.
			return nil
		}
		return fmt.Errorf("failed to create network policy %s: %w", np.Name, err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterNetworkPolicyCreated",
		"Created NetworkPolicy %s for adapter %s (port %d)", np.Name, adapterType, info.Port)

	return nil
}

// updateAdapterNetworkPolicyIfNeeded updates an existing adapter NetworkPolicy if its ingress ports changed.
func (r *DittoServerReconciler) updateAdapterNetworkPolicyIfNeeded(ctx context.Context, ds *dittoiov1alpha1.DittoServer, existing *networkingv1.NetworkPolicy, info AdapterInfo) error {
	adapterType := existing.Labels[adapterTypeLabel]
	desiredPorts := buildAdapterIngressPorts(adapterType, int32(info.Port))

	// Early return if ingress ports already match.
	if ingressPortsMatch(existing.Spec.Ingress, desiredPorts) {
		return nil
	}

	// Re-fetch fresh copy for optimistic locking.
	fresh := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(existing), fresh); err != nil {
		return fmt.Errorf("failed to get fresh network policy: %w", err)
	}

	// Update ingress ports.
	fresh.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: desiredPorts,
		},
	}

	if err := r.Update(ctx, fresh); err != nil {
		return fmt.Errorf("failed to update network policy: %w", err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterNetworkPolicyUpdated",
		"Updated NetworkPolicy %s for adapter %s (%s)", fresh.Name, adapterType, formatIngressPorts(desiredPorts))

	return nil
}

// deleteAdapterNetworkPolicy deletes a K8s NetworkPolicy for a stopped/removed adapter.
func (r *DittoServerReconciler) deleteAdapterNetworkPolicy(ctx context.Context, ds *dittoiov1alpha1.DittoServer, np *networkingv1.NetworkPolicy, adapterType string) error {
	if err := r.Delete(ctx, np); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete network policy %s: %w", np.Name, err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterNetworkPolicyDeleted",
		"Deleted NetworkPolicy %s for adapter %s", np.Name, adapterType)

	return nil
}
