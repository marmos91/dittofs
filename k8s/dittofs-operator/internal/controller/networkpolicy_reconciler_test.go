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

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newAdapterNetworkPolicy creates an adapter NetworkPolicy for testing.
// Delegates to the production builder for consistency.
func newAdapterNetworkPolicy(crName, namespace, adapterType string, port int32) *networkingv1.NetworkPolicy {
	return buildAdapterNetworkPolicy(crName, namespace, adapterType, port)
}

// newStaticNetworkPolicy creates a NetworkPolicy without adapter labels for testing.
func newStaticNetworkPolicy(name, namespace, crName string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":      "dittofs-server",
				"instance": crName,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":      "dittofs-server",
					"instance": crName,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
		},
	}
}

// listAdapterNetworkPolicies returns all adapter NetworkPolicies for a given CR.
func listAdapterNetworkPolicies(t *testing.T, r *DittoServerReconciler, namespace, crName string) []networkingv1.NetworkPolicy {
	t.Helper()
	var npList networkingv1.NetworkPolicyList
	err := r.List(context.Background(), &npList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			adapterNetworkPolicyLabel: "true",
			"instance":                crName,
		},
	)
	if err != nil {
		t.Fatalf("Failed to list adapter network policies: %v", err)
	}
	return npList.Items
}

func TestReconcileNetworkPolicies_NilAdapters_Skips(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// No lastKnownAdapters set (nil = no poll yet)

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify no NetworkPolicies created.
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 0 {
		t.Errorf("Expected 0 adapter network policies, got %d", len(nps))
	}
}

func TestReconcileNetworkPolicies_EmptyAdapters_DeletesOrphans(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create an adapter NetworkPolicy that should be deleted.
	orphanNP := newAdapterNetworkPolicy("test-server", "default", "nfs", 12049)

	r := setupAuthReconciler(t, ds, orphanNP)

	// Set empty adapter list (legitimate -- all adapters stopped).
	r.setLastKnownAdapters(ds, []AdapterInfo{})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify the orphan NetworkPolicy was deleted.
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 0 {
		t.Errorf("Expected 0 adapter network policies after cleanup, got %d", len(nps))
	}
}

func TestReconcileNetworkPolicies_CreatesForRunningAdapter(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// Set adapters: NFS enabled+running.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify one NetworkPolicy was created.
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 1 {
		t.Fatalf("Expected 1 adapter network policy, got %d", len(nps))
	}

	np := nps[0]

	// Verify name.
	if np.Name != "test-server-adapter-nfs" {
		t.Errorf("Expected NetworkPolicy name 'test-server-adapter-nfs', got %s", np.Name)
	}

	// Verify labels.
	if np.Labels[adapterNetworkPolicyLabel] != "true" {
		t.Errorf("Missing adapter-networkpolicy label")
	}
	if np.Labels[adapterTypeLabel] != "nfs" {
		t.Errorf("Expected adapter-type label 'nfs', got '%s'", np.Labels[adapterTypeLabel])
	}
	if np.Labels["app"] != "dittofs-server" {
		t.Errorf("Missing app label")
	}
	if np.Labels["instance"] != "test-server" {
		t.Errorf("Missing instance label")
	}

	// Verify PodSelector.
	if np.Spec.PodSelector.MatchLabels["app"] != "dittofs-server" {
		t.Errorf("PodSelector app label mismatch")
	}
	if np.Spec.PodSelector.MatchLabels["instance"] != "test-server" {
		t.Errorf("PodSelector instance label mismatch")
	}

	// Verify PolicyTypes.
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("Expected PolicyTypes [Ingress], got %v", np.Spec.PolicyTypes)
	}

	// Verify Ingress rules (NFS gets 3 ports: NFS TCP + portmapper TCP + portmapper UDP).
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("Expected 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.Ingress[0].Ports) != 3 {
		t.Fatalf("Expected 3 ingress ports (NFS + portmapper TCP/UDP), got %d", len(np.Spec.Ingress[0].Ports))
	}
	ingressPort := np.Spec.Ingress[0].Ports[0]
	if ingressPort.Port == nil || ingressPort.Port.IntVal != 12049 {
		t.Errorf("Expected first ingress port 12049, got %v", ingressPort.Port)
	}
	if ingressPort.Protocol == nil || *ingressPort.Protocol != "TCP" {
		t.Errorf("Expected NFS protocol TCP, got %v", ingressPort.Protocol)
	}
	portmapTCP := np.Spec.Ingress[0].Ports[1]
	if portmapTCP.Port == nil || portmapTCP.Port.IntVal != portmapperContainerPort {
		t.Errorf("Expected portmapper TCP port %d, got %v", portmapperContainerPort, portmapTCP.Port)
	}
	if portmapTCP.Protocol == nil || *portmapTCP.Protocol != "TCP" {
		t.Errorf("Expected portmapper TCP protocol, got %v", portmapTCP.Protocol)
	}
	portmapUDP := np.Spec.Ingress[0].Ports[2]
	if portmapUDP.Port == nil || portmapUDP.Port.IntVal != portmapperContainerPort {
		t.Errorf("Expected portmapper UDP port %d, got %v", portmapperContainerPort, portmapUDP.Port)
	}
	if portmapUDP.Protocol == nil || *portmapUDP.Protocol != "UDP" {
		t.Errorf("Expected portmapper UDP protocol, got %v", portmapUDP.Protocol)
	}
}

func TestReconcileNetworkPolicies_MultipleAdapters(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// Set adapters: NFS and SMB both enabled+running.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
		{Type: "smb", Enabled: true, Running: true, Port: 12445},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify two NetworkPolicies were created.
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 2 {
		t.Fatalf("Expected 2 adapter network policies, got %d", len(nps))
	}

	// Verify each has the correct adapter type.
	npByType := make(map[string]*networkingv1.NetworkPolicy)
	for i := range nps {
		np := &nps[i]
		adapterType := np.Labels[adapterTypeLabel]
		npByType[adapterType] = np
	}

	nfsNP, ok := npByType["nfs"]
	if !ok {
		t.Fatal("NFS NetworkPolicy not found")
	}
	if nfsNP.Name != "test-server-adapter-nfs" {
		t.Errorf("Expected NFS NetworkPolicy name 'test-server-adapter-nfs', got %s", nfsNP.Name)
	}
	// NFS should have 3 ingress ports (NFS + portmapper TCP/UDP).
	if len(nfsNP.Spec.Ingress) > 0 {
		if len(nfsNP.Spec.Ingress[0].Ports) != 3 {
			t.Errorf("Expected NFS NetworkPolicy to have 3 ingress ports, got %d", len(nfsNP.Spec.Ingress[0].Ports))
		}
		if nfsNP.Spec.Ingress[0].Ports[0].Port.IntVal != 12049 {
			t.Errorf("Expected NFS port 12049, got %d", nfsNP.Spec.Ingress[0].Ports[0].Port.IntVal)
		}
	}

	smbNP, ok := npByType["smb"]
	if !ok {
		t.Fatal("SMB NetworkPolicy not found")
	}
	if smbNP.Name != "test-server-adapter-smb" {
		t.Errorf("Expected SMB NetworkPolicy name 'test-server-adapter-smb', got %s", smbNP.Name)
	}
	// SMB should have 1 ingress port (no portmapper).
	if len(smbNP.Spec.Ingress) > 0 {
		if len(smbNP.Spec.Ingress[0].Ports) != 1 {
			t.Errorf("Expected SMB NetworkPolicy to have 1 ingress port, got %d", len(smbNP.Spec.Ingress[0].Ports))
		}
		if smbNP.Spec.Ingress[0].Ports[0].Port.IntVal != 12445 {
			t.Errorf("Expected SMB port 12445, got %d", smbNP.Spec.Ingress[0].Ports[0].Port.IntVal)
		}
	}
}

func TestReconcileNetworkPolicies_DeletesWhenAdapterStops(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create NFS NetworkPolicy.
	nfsNP := newAdapterNetworkPolicy("test-server", "default", "nfs", 12049)

	r := setupAuthReconciler(t, ds, nfsNP)

	// Set empty adapters (all adapters removed).
	r.setLastKnownAdapters(ds, []AdapterInfo{})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify NFS NetworkPolicy was deleted.
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 0 {
		t.Errorf("Expected 0 adapter network policies after deletion, got %d", len(nps))
	}
}

func TestReconcileNetworkPolicies_UpdatesWhenPortChanges(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create NFS NetworkPolicy with old port 12049.
	nfsNP := newAdapterNetworkPolicy("test-server", "default", "nfs", 12049)

	r := setupAuthReconciler(t, ds, nfsNP)

	// Set adapters with NFS port changed to 2049.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 2049},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify the NetworkPolicy was updated with new port (NFS has 2 ingress ports).
	updated := &networkingv1.NetworkPolicy{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-server-adapter-nfs",
	}, updated)
	if err != nil {
		t.Fatalf("Failed to get updated NetworkPolicy: %v", err)
	}

	if len(updated.Spec.Ingress) != 1 {
		t.Fatalf("Expected 1 ingress rule, got %d", len(updated.Spec.Ingress))
	}
	if len(updated.Spec.Ingress[0].Ports) != 3 {
		t.Fatalf("Expected 3 ingress ports (NFS + portmapper TCP/UDP), got %d", len(updated.Spec.Ingress[0].Ports))
	}
	if updated.Spec.Ingress[0].Ports[0].Port.IntVal != 2049 {
		t.Errorf("Expected NFS port 2049, got %d", updated.Spec.Ingress[0].Ports[0].Port.IntVal)
	}
	if updated.Spec.Ingress[0].Ports[1].Port.IntVal != portmapperContainerPort {
		t.Errorf("Expected portmapper TCP port %d, got %d", portmapperContainerPort, updated.Spec.Ingress[0].Ports[1].Port.IntVal)
	}
	if updated.Spec.Ingress[0].Ports[2].Port.IntVal != portmapperContainerPort {
		t.Errorf("Expected portmapper UDP port %d, got %d", portmapperContainerPort, updated.Spec.Ingress[0].Ports[2].Port.IntVal)
	}
}

func TestReconcileNetworkPolicies_IgnoresDisabledAdapters(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// NFS enabled+running, SMB enabled but NOT running, gRPC disabled.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
		{Type: "smb", Enabled: true, Running: false, Port: 12445},
		{Type: "grpc", Enabled: false, Running: false, Port: 50051},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify only NFS NetworkPolicy created (not SMB or gRPC).
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 1 {
		t.Fatalf("Expected 1 adapter network policy (NFS only), got %d", len(nps))
	}
	if nps[0].Labels[adapterTypeLabel] != "nfs" {
		t.Errorf("Expected adapter type 'nfs', got '%s'", nps[0].Labels[adapterTypeLabel])
	}
}

func TestReconcileNetworkPolicies_DoesNotTouchStaticResources(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create a static NetworkPolicy without the adapterNetworkPolicyLabel.
	staticNP := newStaticNetworkPolicy("test-server-default-deny", "default", "test-server")

	r := setupAuthReconciler(t, ds, staticNP)

	// Set empty adapter list so the reconciler would try to delete orphaned NetworkPolicies.
	r.setLastKnownAdapters(ds, []AdapterInfo{})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify the static NetworkPolicy still exists.
	check := &networkingv1.NetworkPolicy{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-server-default-deny",
	}, check)
	if err != nil {
		t.Errorf("Static NetworkPolicy should still exist: %v", err)
	}
}

func TestReconcileNetworkPolicies_OwnerReferenceSet(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify NetworkPolicy has owner reference.
	nps := listAdapterNetworkPolicies(t, r, "default", "test-server")
	if len(nps) != 1 {
		t.Fatalf("Expected 1 network policy, got %d", len(nps))
	}

	np := nps[0]
	if len(np.OwnerReferences) == 0 {
		t.Errorf("NetworkPolicy %s has no owner references", np.Name)
	} else if np.OwnerReferences[0].Kind != "DittoServer" {
		t.Errorf("NetworkPolicy %s owner reference kind = %s, want DittoServer", np.Name, np.OwnerReferences[0].Kind)
	}
}

func TestReconcileNetworkPolicies_BaselineCreated(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify baseline NetworkPolicy exists allowing API port.
	baselineNP := &networkingv1.NetworkPolicy{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      baselineNetworkPolicyName("test-server"),
	}, baselineNP)
	if err != nil {
		t.Fatalf("Baseline NetworkPolicy not found: %v", err)
	}

	if baselineNP.Labels[baselineNetworkPolicyLabel] != "true" {
		t.Errorf("Baseline NetworkPolicy missing label %s", baselineNetworkPolicyLabel)
	}

	if len(baselineNP.Spec.Ingress) == 0 || len(baselineNP.Spec.Ingress[0].Ports) == 0 || baselineNP.Spec.Ingress[0].Ports[0].Port == nil {
		t.Fatal("Baseline NetworkPolicy has no ingress ports")
	}
	if port := baselineNP.Spec.Ingress[0].Ports[0].Port.IntVal; port != defaultAPIPort {
		t.Errorf("Baseline NetworkPolicy port = %d, want %d", port, defaultAPIPort)
	}

	if len(baselineNP.OwnerReferences) == 0 {
		t.Errorf("Baseline NetworkPolicy has no owner references")
	}
}

func TestReconcileNetworkPolicies_BaselineCreatedEvenWithNilAdapters(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// No adapters set (nil = no poll yet).
	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Baseline should still be created even when adapter reconciliation is skipped.
	baselineNP := &networkingv1.NetworkPolicy{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      baselineNetworkPolicyName("test-server"),
	}, baselineNP)
	if err != nil {
		t.Fatalf("Baseline NetworkPolicy not found even with nil adapters: %v", err)
	}
}

func TestBuildAdapterNetworkPolicy_NFS_MultiPort(t *testing.T) {
	np := buildAdapterNetworkPolicy("test-server", "default", "nfs", 12049)

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("Expected 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.Ingress[0].Ports) != 3 {
		t.Fatalf("Expected 3 ingress ports for NFS (NFS + portmapper TCP/UDP), got %d", len(np.Spec.Ingress[0].Ports))
	}

	// First port: NFS (TCP)
	if np.Spec.Ingress[0].Ports[0].Port.IntVal != 12049 {
		t.Errorf("Expected first port 12049, got %d", np.Spec.Ingress[0].Ports[0].Port.IntVal)
	}

	// Second port: portmapper TCP
	if np.Spec.Ingress[0].Ports[1].Port.IntVal != portmapperContainerPort {
		t.Errorf("Expected portmapper TCP port %d, got %d", portmapperContainerPort, np.Spec.Ingress[0].Ports[1].Port.IntVal)
	}
	if np.Spec.Ingress[0].Ports[1].Protocol == nil || *np.Spec.Ingress[0].Ports[1].Protocol != "TCP" {
		t.Errorf("Expected portmapper TCP protocol, got %v", np.Spec.Ingress[0].Ports[1].Protocol)
	}

	// Third port: portmapper UDP
	if np.Spec.Ingress[0].Ports[2].Port.IntVal != portmapperContainerPort {
		t.Errorf("Expected portmapper UDP port %d, got %d", portmapperContainerPort, np.Spec.Ingress[0].Ports[2].Port.IntVal)
	}
	if np.Spec.Ingress[0].Ports[2].Protocol == nil || *np.Spec.Ingress[0].Ports[2].Protocol != "UDP" {
		t.Errorf("Expected portmapper UDP protocol, got %v", np.Spec.Ingress[0].Ports[2].Protocol)
	}
}

func TestBuildAdapterNetworkPolicy_SMB_SinglePort(t *testing.T) {
	np := buildAdapterNetworkPolicy("test-server", "default", "smb", 12445)

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("Expected 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("Expected 1 ingress port for SMB (no portmapper), got %d", len(np.Spec.Ingress[0].Ports))
	}
	if np.Spec.Ingress[0].Ports[0].Port.IntVal != 12445 {
		t.Errorf("Expected port 12445, got %d", np.Spec.Ingress[0].Ports[0].Port.IntVal)
	}
}

func TestUpdateNetworkPolicy_NFS_SinglePortToMultiPort(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Pre-create NFS NetworkPolicy with only NFS port (old format, no portmapper).
	oldNP := buildAdapterNetworkPolicy("test-server", "default", "smb", 12049)
	// Override: set labels and name to look like an NFS NP with old single-port format.
	oldNP.Name = adapterResourceName("test-server", "nfs")
	oldNP.Labels = networkPolicyLabels("test-server", "nfs")

	r := setupAuthReconciler(t, ds, oldNP)

	// Set NFS adapter as active.
	r.setLastKnownAdapters(ds, []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
	})

	err := r.reconcileNetworkPolicies(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileNetworkPolicies returned error: %v", err)
	}

	// Verify the NetworkPolicy now has 3 ports.
	updated := &networkingv1.NetworkPolicy{}
	err = r.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      adapterResourceName("test-server", "nfs"),
	}, updated)
	if err != nil {
		t.Fatalf("Failed to get updated NetworkPolicy: %v", err)
	}

	if len(updated.Spec.Ingress) != 1 {
		t.Fatalf("Expected 1 ingress rule, got %d", len(updated.Spec.Ingress))
	}
	if len(updated.Spec.Ingress[0].Ports) != 3 {
		t.Fatalf("Expected 3 ingress ports after update (NFS + portmapper TCP/UDP), got %d", len(updated.Spec.Ingress[0].Ports))
	}
}
