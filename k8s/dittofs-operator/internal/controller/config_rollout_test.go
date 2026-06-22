package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/resources"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func baseDittoServer() *v1alpha1.DittoServer {
	ds := &v1alpha1.DittoServer{}
	ds.Name = "demo"
	ds.Namespace = "default"
	ds.Generation = 1
	ds.Spec.Image = "dittofs:test"
	ds.Spec.Storage.MetadataSize = "5Gi"
	return ds
}

func getSTS(t *testing.T, r *DittoServerReconciler) *appsv1.StatefulSet {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, sts); err != nil {
		t.Fatalf("get sts: %v", err)
	}
	return sts
}

// TestLoggingChangeRollsPod is the #1319 acceptance: a config-only logging change
// re-renders the config and flips the pod-template config-hash annotation so the
// StatefulSet rolls. Generation bumps on the spec edit, which alone is enough.
func TestLoggingChangeRollsPod(t *testing.T) {
	ds := baseDittoServer()
	r := setupDittoServerReconciler(t, fields{dittoServer: ds})
	ctx := context.Background()

	if _, err := r.reconcileStatefulSet(ctx, ds, 1); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	ann1 := getSTS(t, r).Spec.Template.Annotations[resources.ConfigHashAnnotation]

	// Operator-side edit: set DEBUG and bump generation (kubectl would).
	ds.Spec.Logging = &v1alpha1.LoggingSpec{Level: "DEBUG"}
	ds.Generation = 2

	if _, err := r.reconcileStatefulSet(ctx, ds, 1); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	ann2 := getSTS(t, r).Spec.Template.Annotations[resources.ConfigHashAnnotation]

	if ann1 == "" || ann2 == "" {
		t.Fatal("config-hash annotation missing")
	}
	if ann1 == ann2 {
		t.Errorf("config-hash unchanged after logging edit (%s); pod would NOT roll", ann1)
	}
}

// TestKerberosKeytabMounted verifies the keytab Secret is mounted read-only at the
// fixed path the rendered keytab_path points at, and that a keytab rotation
// (Secret data change) flips the config-hash so the pod rolls.
func TestKerberosKeytabMounted(t *testing.T) {
	ds := baseDittoServer()
	ds.Spec.Identity = &v1alpha1.IdentityConfig{
		Kerberos: &v1alpha1.KerberosConfig{
			Enabled:          true,
			ServicePrincipal: "nfs/demo@EXAMPLE.COM",
			KeytabSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "krb-keytab"},
				Key:                  "dittofs.keytab",
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "krb-keytab", Namespace: "default"},
		Data:       map[string][]byte{"dittofs.keytab": []byte("KEYTAB-V1")},
	}
	// The managed JWT secret must exist or collectSecretData errors out before it
	// folds the keytab into the hash (the controller then hashes config-only).
	jwt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ds.GetManagedJWTSecretName(), Namespace: "default"},
		Data:       map[string][]byte{v1alpha1.ManagedJWTSecretKey: []byte("jwt")},
	}
	r := setupDittoServerReconciler(t, fields{dittoServer: ds, secrets: []*corev1.Secret{secret, jwt}})
	ctx := context.Background()

	h1, err := r.reconcileStatefulSet(ctx, ds, 1)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	sts := getSTS(t, r)

	// Volume present.
	var foundVol bool
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "kerberos-keytab" {
			foundVol = true
			if v.Secret == nil || v.Secret.SecretName != "krb-keytab" {
				t.Fatalf("keytab volume points at wrong secret: %+v", v.Secret)
			}
		}
	}
	if !foundVol {
		t.Fatal("kerberos-keytab volume not found on pod template")
	}

	// Mount present and read-only at the expected path.
	var foundMount bool
	for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "kerberos-keytab" {
			foundMount = true
			if m.MountPath != v1alpha1.KerberosKeytabMountPath || !m.ReadOnly {
				t.Fatalf("keytab mount wrong: %+v", m)
			}
		}
	}
	if !foundMount {
		t.Fatal("kerberos-keytab volume mount not found")
	}

	// Keytab rotation: same generation, but the Secret bytes change -> hash flips.
	secret.Data["dittofs.keytab"] = []byte("KEYTAB-V2")
	if err := r.Update(ctx, secret); err != nil {
		t.Fatalf("update secret: %v", err)
	}
	h2, err := r.reconcileStatefulSet(ctx, ds, 1)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if h1 == h2 {
		t.Error("config-hash unchanged after keytab rotation; pod would NOT roll")
	}
}
