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

const (
	// adapterNetworkPolicyLabel marks a NetworkPolicy as managed by the adapter NetworkPolicy reconciler.
	adapterNetworkPolicyLabel = "dittofs.io/adapter-networkpolicy"

	// baselineNetworkPolicyLabel marks the baseline NetworkPolicy that allows API/health traffic.
	baselineNetworkPolicyLabel = "dittofs.io/baseline-networkpolicy"
)

// networkPolicyLabels returns the labels for an adapter NetworkPolicy.
func networkPolicyLabels(crName, adapterType string) map[string]string {
	labels := podSelectorLabels(crName)
	labels[adapterNetworkPolicyLabel] = "true"
	labels[adapterTypeLabel] = sanitizeAdapterType(adapterType)
	return labels
}

// currentIngressPort extracts the first ingress port from a NetworkPolicy, or 0 if absent.
func currentIngressPort(np *networkingv1.NetworkPolicy) int32 {
	if len(np.Spec.Ingress) > 0 && len(np.Spec.Ingress[0].Ports) > 0 && np.Spec.Ingress[0].Ports[0].Port != nil {
		return np.Spec.Ingress[0].Ports[0].Port.IntVal
	}
	return 0
}

// buildAdapterNetworkPolicy constructs a NetworkPolicy allowing TCP ingress on a single adapter port.
func buildAdapterNetworkPolicy(crName, namespace, adapterType string, port int32) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
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
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: port,
							},
						},
					},
				},
			},
		},
	}
}

// baselineNetworkPolicyName returns the name for the baseline NetworkPolicy.
func baselineNetworkPolicyName(crName string) string {
	return crName + "-baseline"
}

// ensureBaselineNetworkPolicy ensures a NetworkPolicy exists that allows API port traffic.
// This prevents adapter NetworkPolicies from blocking operator-to-API and health check traffic.
func (r *DittoServerReconciler) ensureBaselineNetworkPolicy(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	npName := baselineNetworkPolicyName(ds.Name)
	apiPort := getAPIPort(ds)
	tcp := corev1.ProtocolTCP

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npName,
			Namespace: ds.Namespace,
			Labels: map[string]string{
				"app":                    "dittofs-server",
				"instance":               ds.Name,
				baselineNetworkPolicyLabel: "true",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: podSelectorLabels(ds.Name),
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: apiPort,
							},
						},
					},
				},
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

	// Update port if it changed.
	if currentIngressPort(existing) != apiPort {
		existing.Spec.Ingress = np.Spec.Ingress
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update baseline network policy: %w", err)
		}
	}

	return nil
}

// reconcileNetworkPolicies synchronizes K8s NetworkPolicies with the discovered adapter state.
// It creates NetworkPolicies for enabled+running adapters, updates when ports change,
// and deletes NetworkPolicies for stopped/removed adapters. Only manages NetworkPolicies
// with the dittofs.io/adapter-networkpolicy label -- never touches static NetworkPolicies.
// Unlike adapter Services, NetworkPolicy errors are propagated (security-critical).
func (r *DittoServerReconciler) reconcileNetworkPolicies(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	logger := logf.FromContext(ctx)

	// Ensure baseline NetworkPolicy allowing API port before creating adapter policies.
	// This prevents adapter policies from blocking operator-to-API and health check traffic.
	if err := r.ensureBaselineNetworkPolicy(ctx, ds); err != nil {
		return fmt.Errorf("failed to ensure baseline network policy: %w", err)
	}

	// DISC-03 safety: if no successful poll has occurred yet, skip entirely.
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
			adapterNetworkPolicyLabel: "true",
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

// updateAdapterNetworkPolicyIfNeeded updates an existing adapter NetworkPolicy if its port changed.
func (r *DittoServerReconciler) updateAdapterNetworkPolicyIfNeeded(ctx context.Context, ds *dittoiov1alpha1.DittoServer, existing *networkingv1.NetworkPolicy, info AdapterInfo) error {
	desiredPort := int32(info.Port)

	// Early return if port already matches.
	if currentIngressPort(existing) == desiredPort {
		return nil
	}

	// Re-fetch fresh copy for optimistic locking.
	fresh := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(existing), fresh); err != nil {
		return fmt.Errorf("failed to get fresh network policy: %w", err)
	}

	adapterType := fresh.Labels[adapterTypeLabel]
	oldPort := currentIngressPort(fresh)

	// Update ingress port.
	tcp := corev1.ProtocolTCP
	fresh.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{
					Protocol: &tcp,
					Port: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: desiredPort,
					},
				},
			},
		},
	}

	err := retryOnConflict(func() error {
		return r.Update(ctx, fresh)
	})
	if err != nil {
		return fmt.Errorf("failed to update network policy: %w", err)
	}

	r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterNetworkPolicyUpdated",
		"Updated NetworkPolicy %s for adapter %s (port %d -> %d)", fresh.Name, adapterType, oldPort, desiredPort)

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
