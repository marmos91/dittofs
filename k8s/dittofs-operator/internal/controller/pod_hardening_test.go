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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// TestGetContainerSecurityContext_SecureDefault asserts that when the user does
// not supply a container SecurityContext, the operator applies a secure default
// (drop ALL caps, no privilege escalation, run as non-root, RuntimeDefault
// seccomp) so the StatefulSet is admitted under a "restricted" Pod-Security
// namespace. readOnlyRootFilesystem must stay unset because dfs writes to
// os.TempDir outside its mounted volumes.
func TestGetContainerSecurityContext_SecureDefault(t *testing.T) {
	ds := &v1alpha1.DittoServer{}

	sc := getContainerSecurityContext(ds)
	if sc == nil {
		t.Fatalf("expected a non-nil SecurityContext when Spec.SecurityContext is nil")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("expected AllowPrivilegeEscalation=false, got %v", sc.AllowPrivilegeEscalation)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("expected RunAsNonRoot=true, got %v", sc.RunAsNonRoot)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != capabilityAll {
		t.Errorf("expected Capabilities.Drop=[ALL], got %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("expected SeccompProfile=RuntimeDefault, got %+v", sc.SeccompProfile)
	}
	if sc.ReadOnlyRootFilesystem != nil {
		t.Errorf("expected ReadOnlyRootFilesystem unset (dfs writes to /tmp), got %v", *sc.ReadOnlyRootFilesystem)
	}
}

// TestGetContainerSecurityContext_UserOverrideMerge asserts that user-supplied
// fields win over the secure defaults while unset fields retain the defaults.
func TestGetContainerSecurityContext_UserOverrideMerge(t *testing.T) {
	ds := &v1alpha1.DittoServer{
		Spec: v1alpha1.DittoServerSpec{
			SecurityContext: &corev1.SecurityContext{
				// User opts into a read-only root filesystem and a specific UID,
				// but leaves the rest to the operator defaults.
				ReadOnlyRootFilesystem: ptr.To(true),
				RunAsUser:              ptr.To(int64(1234)),
			},
		},
	}

	sc := getContainerSecurityContext(ds)
	if sc == nil {
		t.Fatalf("expected a non-nil SecurityContext")
	}
	// User overrides win.
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("expected user ReadOnlyRootFilesystem=true to win, got %v", sc.ReadOnlyRootFilesystem)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 1234 {
		t.Errorf("expected user RunAsUser=1234 to win, got %v", sc.RunAsUser)
	}
	// Unset user fields keep the secure defaults.
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("expected default AllowPrivilegeEscalation=false to persist, got %v", sc.AllowPrivilegeEscalation)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != capabilityAll {
		t.Errorf("expected default Capabilities.Drop=[ALL] to persist, got %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("expected default SeccompProfile=RuntimeDefault to persist, got %+v", sc.SeccompProfile)
	}
}

// TestGetContainerSecurityContext_UserAddsCapabilityKeepsDropBaseline asserts
// that when the user sets Capabilities only to Add a capability (leaving Drop
// unset), the operator still backfills Drop=[ALL] so the pod satisfies the
// restricted Pod-Security-Standard, while preserving the user's Add list.
func TestGetContainerSecurityContext_UserAddsCapabilityKeepsDropBaseline(t *testing.T) {
	ds := &v1alpha1.DittoServer{
		Spec: v1alpha1.DittoServerSpec{
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{
					Add: []corev1.Capability{"NET_BIND_SERVICE"},
				},
			},
		},
	}

	sc := getContainerSecurityContext(ds)
	if sc == nil || sc.Capabilities == nil {
		t.Fatalf("expected a non-nil SecurityContext with Capabilities")
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_BIND_SERVICE" {
		t.Errorf("expected user Capabilities.Add=[NET_BIND_SERVICE] to be preserved, got %+v", sc.Capabilities.Add)
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != capabilityAll {
		t.Errorf("expected backfilled Capabilities.Drop=[ALL], got %+v", sc.Capabilities.Drop)
	}
}

// TestGetContainerSecurityContext_UserDisablesHardening asserts the merge does
// not clobber a user that explicitly relaxes a default (e.g. allows privilege
// escalation) — user always wins, even when relaxing.
func TestGetContainerSecurityContext_UserDisablesHardening(t *testing.T) {
	ds := &v1alpha1.DittoServer{
		Spec: v1alpha1.DittoServerSpec{
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(true),
			},
		},
	}

	sc := getContainerSecurityContext(ds)
	if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Errorf("expected user AllowPrivilegeEscalation=true to win, got %v", sc.AllowPrivilegeEscalation)
	}
}

// TestReconcileStatefulSet_PodHardening reconciles a StatefulSet through the
// fake client and asserts the managed PodSpec disables ServiceAccount-token
// automount (M-SEC-2) and carries the secure default container SecurityContext
// (M-SEC-3).
func TestReconcileStatefulSet_PodHardening(t *testing.T) {
	ctx := context.Background()

	ds := newHardeningDittoServer()
	r := setupDittoServerReconciler(t, fields{dittoServer: ds})

	if _, err := r.reconcileStatefulSet(ctx, ds, 1); err != nil {
		t.Fatalf("reconcileStatefulSet failed: %v", err)
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, sts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}

	spec := sts.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("expected AutomountServiceAccountToken=false, got %v", spec.AutomountServiceAccountToken)
	}

	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(spec.Containers))
	}
	sc := spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatalf("expected a container SecurityContext on the managed pod")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("expected AllowPrivilegeEscalation=false, got %v", sc.AllowPrivilegeEscalation)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("expected RunAsNonRoot=true, got %v", sc.RunAsNonRoot)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != capabilityAll {
		t.Errorf("expected Capabilities.Drop=[ALL], got %+v", sc.Capabilities)
	}
}

func newHardeningDittoServer() *v1alpha1.DittoServer {
	return &v1alpha1.DittoServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harden-test",
			Namespace: "default",
		},
		Spec: v1alpha1.DittoServerSpec{
			Image: "marmos91c/dittofs:test",
			Storage: v1alpha1.StorageSpec{
				MetadataSize: "1Gi",
				CacheSize:    "1Gi",
			},
		},
	}
}
