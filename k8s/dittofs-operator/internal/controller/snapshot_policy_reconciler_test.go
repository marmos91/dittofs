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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/conditions"
)

// forbidden answers as the DittoFS API does when a route needs the admin role.
func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,"detail":"Admin access required"}`))
}

// A refused call shows on the DittoServer, and port 111 is published only once
// the portmapper is seen on.
func TestReconcileAuthenticatedState_Refusals(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.SnapshotPolicies = []dittoiov1alpha1.ShareSnapshotPolicySpec{{Share: "/archive", Interval: "24h"}}
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ds.GetOperatorCredentialsSecretName(), Namespace: "default"},
		Data:       map[string][]byte{"access-token": []byte("operator-token")},
	}
	r := setupAuthReconciler(t, ds, credSecret)

	allowed := false
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/adapters": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]AdapterInfo{{Type: "nfs", Enabled: true, Running: true, Port: 12049}})
		},
		"GET /api/v1/adapters/nfs/settings": func(w http.ResponseWriter, _ *http.Request) {
			if !allowed {
				forbidden(w)
				return
			}
			_, _ = w.Write([]byte(`{"portmapper_enabled":true}`))
		},
		"PUT /api/v1/shares/{share}/snapshot-policy": func(w http.ResponseWriter, _ *http.Request) {
			if !allowed {
				forbidden(w)
			}
		},
	})
	credSecret.Data["server-url"] = []byte(server.URL)
	if err := r.Update(context.Background(), credSecret); err != nil {
		t.Fatalf("update credentials secret: %v", err)
	}

	reconcile := func() (*metav1.Condition, []corev1.ServicePort) {
		t.Helper()
		ctx := context.Background()
		if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
			t.Fatalf("get DittoServer: %v", err)
		}
		if _, err := r.reconcileAuthenticatedState(ctx, ds); err != nil {
			t.Fatalf("reconcileAuthenticatedState: %v", err)
		}
		got := &dittoiov1alpha1.DittoServer{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(ds), got); err != nil {
			t.Fatalf("get DittoServer: %v", err)
		}
		svc := &corev1.Service{}
		key := client.ObjectKey{Namespace: "default", Name: adapterResourceName(ds.Name, "nfs")}
		if err := r.Get(ctx, key, svc); err != nil {
			t.Fatalf("get NFS Service: %v", err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, conditions.ConditionAuthorized), svc.Spec.Ports
	}
	publishes111 := func(ports []corev1.ServicePort) bool {
		for _, p := range ports {
			if p.Port == portmapperServicePort {
				return true
			}
		}
		return false
	}

	c, ports := reconcile()
	if c == nil || c.Status != metav1.ConditionFalse ||
		!strings.Contains(c.Message, "NFS portmapper: Admin access required") ||
		!strings.Contains(c.Message, "snapshot policy for /archive: Admin access required") {
		t.Errorf("refused calls: Authorized = %+v, want False naming the portmapper and /archive's policy", c)
	}
	if publishes111(ports) {
		t.Errorf("refused calls: the NFS Service publishes 111 with the portmapper unseen: %v", servicePortNames(ports))
	}

	allowed = true
	c, ports = reconcile()
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("allowed calls: Authorized = %+v, want True", c)
	}
	if !publishes111(ports) {
		t.Errorf("allowed calls: the NFS Service doesn't publish 111 with the portmapper on: %v", servicePortNames(ports))
	}
}
