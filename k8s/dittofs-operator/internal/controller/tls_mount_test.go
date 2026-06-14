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
)

func newTLSDittoServer(certSecret, clientCASecret string) *v1alpha1.DittoServer {
	cp := &v1alpha1.ControlPlaneAPIConfig{CertSecretName: certSecret}
	if clientCASecret != "" {
		cp.ClientCASecretName = clientCASecret
	}
	return &v1alpha1.DittoServer{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-test", Namespace: "default"},
		Spec: v1alpha1.DittoServerSpec{
			Image:        "marmos91c/dittofs:test",
			ControlPlane: cp,
			Storage: v1alpha1.StorageSpec{
				MetadataSize: "1Gi",
			},
		},
	}
}

func findVolume(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

func findMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

// TestReconcileStatefulSet_NativeTLSCertMount asserts that naming a cert Secret
// makes the operator mount it read-only into the dfs container, switch the
// health probes to HTTPS, and (with no client-CA) NOT mount a client-CA volume.
func TestReconcileStatefulSet_NativeTLSCertMount(t *testing.T) {
	ctx := context.Background()
	ds := newTLSDittoServer("dfs-server-tls", "")
	r := setupDittoServerReconciler(t, fields{dittoServer: ds})

	if _, err := r.reconcileStatefulSet(ctx, ds, 1); err != nil {
		t.Fatalf("reconcileStatefulSet failed: %v", err)
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, sts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}
	spec := sts.Spec.Template.Spec

	// Cert Secret volume present and backed by the named Secret.
	vol := findVolume(spec.Volumes, "tls-cert")
	if vol == nil {
		t.Fatal("expected a tls-cert volume")
	}
	if vol.Secret == nil || vol.Secret.SecretName != "dfs-server-tls" {
		t.Errorf("tls-cert volume = %+v, want Secret source dfs-server-tls", vol.VolumeSource)
	}

	// Read-only mount at the expected path.
	mount := findMount(spec.Containers[0].VolumeMounts, "tls-cert")
	if mount == nil {
		t.Fatal("expected a tls-cert volumeMount on the dfs container")
	}
	if !mount.ReadOnly {
		t.Error("tls-cert mount must be read-only")
	}
	if mount.MountPath != v1alpha1.TLSCertMountPath {
		t.Errorf("tls-cert mountPath = %q, want %q", mount.MountPath, v1alpha1.TLSCertMountPath)
	}

	// No client-CA volume/mount without a client-CA Secret.
	if findVolume(spec.Volumes, "tls-client-ca") != nil {
		t.Error("did not expect a tls-client-ca volume without a client-CA secret")
	}

	// Probes must switch to HTTPS since the pod now serves TLS only.
	c := spec.Containers[0]
	for _, p := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		if p == nil || p.HTTPGet == nil {
			t.Fatal("expected HTTPGet probes")
		}
		if p.HTTPGet.Scheme != corev1.URISchemeHTTPS {
			t.Errorf("probe %s scheme = %q, want HTTPS", p.HTTPGet.Path, p.HTTPGet.Scheme)
		}
	}
}

// TestReconcileStatefulSet_MutualTLSClientCAMount asserts that adding a
// client-CA Secret mounts a second read-only volume for the CA bundle.
func TestReconcileStatefulSet_MutualTLSClientCAMount(t *testing.T) {
	ctx := context.Background()
	ds := newTLSDittoServer("dfs-server-tls", "dfs-client-ca")
	r := setupDittoServerReconciler(t, fields{dittoServer: ds})

	if _, err := r.reconcileStatefulSet(ctx, ds, 1); err != nil {
		t.Fatalf("reconcileStatefulSet failed: %v", err)
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, sts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}
	spec := sts.Spec.Template.Spec

	vol := findVolume(spec.Volumes, "tls-client-ca")
	if vol == nil {
		t.Fatal("expected a tls-client-ca volume for mTLS")
	}
	if vol.Secret == nil || vol.Secret.SecretName != "dfs-client-ca" {
		t.Errorf("tls-client-ca volume = %+v, want Secret source dfs-client-ca", vol.VolumeSource)
	}
	mount := findMount(spec.Containers[0].VolumeMounts, "tls-client-ca")
	if mount == nil || !mount.ReadOnly || mount.MountPath != v1alpha1.TLSClientCAMountPath {
		t.Errorf("tls-client-ca mount = %+v, want read-only at %q", mount, v1alpha1.TLSClientCAMountPath)
	}

	// Under mTLS the kubelet cannot present a client cert, so HTTPS HTTPGet
	// probes would fail the handshake. The probes must fall back to TCPSocket.
	c := spec.Containers[0]
	for _, p := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		if p == nil || p.TCPSocket == nil {
			t.Errorf("expected a TCPSocket probe under mTLS, got %+v", p)
		}
		if p != nil && p.HTTPGet != nil {
			t.Error("mTLS probe must not be HTTPGet (handshake needs a client cert)")
		}
	}
}

// TestReconcileStatefulSet_NoTLSNoCertMount asserts the default (no cert Secret)
// path mounts neither TLS volume and keeps HTTP probes — preserving back-compat.
func TestReconcileStatefulSet_NoTLSNoCertMount(t *testing.T) {
	ctx := context.Background()
	ds := newHardeningDittoServer() // no ControlPlane TLS
	r := setupDittoServerReconciler(t, fields{dittoServer: ds})

	if _, err := r.reconcileStatefulSet(ctx, ds, 1); err != nil {
		t.Fatalf("reconcileStatefulSet failed: %v", err)
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, sts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}
	spec := sts.Spec.Template.Spec

	if findVolume(spec.Volumes, "tls-cert") != nil || findVolume(spec.Volumes, "tls-client-ca") != nil {
		t.Error("expected no TLS volumes when no cert secret is configured")
	}
	if p := spec.Containers[0].LivenessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("expected HTTP liveness probe by default, got %+v", p)
	}
}
