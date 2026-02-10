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
	"testing"
	"time"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestReconcileAdapters_Success(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create operator credentials Secret pointing to mock server
	// Server URL will be set after server starts
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username":      []byte("k8s-operator"),
			"password":      []byte("operator-pass"),
			"access-token":  []byte("test-access-token"),
			"refresh-token": []byte("test-refresh-token"),
			"server-url":    []byte("http://placeholder"),
		},
	}

	r := setupAuthReconciler(t, ds, credSecret)

	// Mock DittoFS API returning 2 adapters
	adapters := []AdapterInfo{
		{Type: "nfs", Enabled: true, Running: true, Port: 12049},
		{Type: "smb", Enabled: true, Running: true, Port: 12445},
	}
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/adapters": func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(adapters)
		},
	})

	// Update the secret with the correct server URL
	credSecret.Data["server-url"] = []byte(server.URL)
	if err := r.Update(context.Background(), credSecret); err != nil {
		t.Fatalf("Failed to update credentials secret with server URL: %v", err)
	}

	result, err := r.reconcileAdapters(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapters returned error: %v", err)
	}

	// Verify RequeueAfter is the default polling interval (30s)
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("Expected RequeueAfter = 30s, got %v", result.RequeueAfter)
	}

	// Verify adapters were stored
	stored := r.getLastKnownAdapters(ds)
	if stored == nil {
		t.Fatal("Expected lastKnownAdapters to be set, got nil")
	}
	if len(stored) != 2 {
		t.Fatalf("Expected 2 adapters, got %d", len(stored))
	}
	if stored[0].Type != "nfs" || stored[0].Port != 12049 {
		t.Errorf("First adapter: expected nfs:12049, got %s:%d", stored[0].Type, stored[0].Port)
	}
	if stored[1].Type != "smb" || stored[1].Port != 12445 {
		t.Errorf("Second adapter: expected smb:12445, got %s:%d", stored[1].Type, stored[1].Port)
	}
}

func TestReconcileAdapters_APIError_PreservesState(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username":      []byte("k8s-operator"),
			"password":      []byte("operator-pass"),
			"access-token":  []byte("test-access-token"),
			"refresh-token": []byte("test-refresh-token"),
			"server-url":    []byte("http://placeholder"),
		},
	}

	r := setupAuthReconciler(t, ds, credSecret)

	// Pre-populate lastKnownAdapters with 2 adapters
	r.lastKnownAdapters = map[string][]AdapterInfo{
		"default/test-server": {
			{Type: "nfs", Enabled: true, Running: true, Port: 12049},
			{Type: "smb", Enabled: true, Running: true, Port: 12445},
		},
	}

	// Mock DittoFS API returning 500
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/adapters": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":"INTERNAL","message":"Server error"}`))
		},
	})

	credSecret.Data["server-url"] = []byte(server.URL)
	if err := r.Update(context.Background(), credSecret); err != nil {
		t.Fatalf("Failed to update credentials secret: %v", err)
	}

	result, err := r.reconcileAdapters(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapters should not return error on API failure, got: %v", err)
	}

	// Should still requeue
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("Expected RequeueAfter = 30s, got %v", result.RequeueAfter)
	}

	// DISC-03: Verify adapters are preserved
	stored := r.getLastKnownAdapters(ds)
	if stored == nil {
		t.Fatal("Expected lastKnownAdapters to be preserved, got nil")
	}
	if len(stored) != 2 {
		t.Fatalf("Expected 2 adapters preserved (DISC-03), got %d", len(stored))
	}
}

func TestReconcileAdapters_EmptyResponse_StoresEmpty(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username":      []byte("k8s-operator"),
			"password":      []byte("operator-pass"),
			"access-token":  []byte("test-access-token"),
			"refresh-token": []byte("test-refresh-token"),
			"server-url":    []byte("http://placeholder"),
		},
	}

	r := setupAuthReconciler(t, ds, credSecret)

	// Mock DittoFS API returning empty array
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/adapters": func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
		},
	})

	credSecret.Data["server-url"] = []byte(server.URL)
	if err := r.Update(context.Background(), credSecret); err != nil {
		t.Fatalf("Failed to update credentials secret: %v", err)
	}

	result, err := r.reconcileAdapters(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapters returned error: %v", err)
	}

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("Expected RequeueAfter = 30s, got %v", result.RequeueAfter)
	}

	// Verify empty slice is stored (not nil)
	stored := r.getLastKnownAdapters(ds)
	if stored == nil {
		t.Fatal("Expected lastKnownAdapters to be empty slice, got nil")
	}
	if len(stored) != 0 {
		t.Errorf("Expected 0 adapters, got %d", len(stored))
	}
}

func TestReconcileAdapters_NoCredentials_PreservesState(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// No credentials Secret created
	r := setupAuthReconciler(t, ds)

	result, err := r.reconcileAdapters(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapters should not return error when no credentials, got: %v", err)
	}

	// Should still requeue
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("Expected RequeueAfter = 30s, got %v", result.RequeueAfter)
	}

	// Should not have stored anything
	stored := r.getLastKnownAdapters(ds)
	if stored != nil {
		t.Errorf("Expected nil lastKnownAdapters (no successful poll), got %v", stored)
	}
}

func TestReconcileAdapters_CustomPollingInterval(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterDiscovery = &dittoiov1alpha1.AdapterDiscoverySpec{
		PollingInterval: "1m",
	}

	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username":      []byte("k8s-operator"),
			"password":      []byte("operator-pass"),
			"access-token":  []byte("test-access-token"),
			"refresh-token": []byte("test-refresh-token"),
			"server-url":    []byte("http://placeholder"),
		},
	}

	r := setupAuthReconciler(t, ds, credSecret)

	// Mock DittoFS API
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/adapters": func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
		},
	})

	credSecret.Data["server-url"] = []byte(server.URL)
	if err := r.Update(context.Background(), credSecret); err != nil {
		t.Fatalf("Failed to update credentials secret: %v", err)
	}

	result, err := r.reconcileAdapters(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdapters returned error: %v", err)
	}

	// Verify custom polling interval (1m = 60s)
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("Expected RequeueAfter = 60s, got %v", result.RequeueAfter)
	}
}

func TestGetPollingInterval_Default(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	// No AdapterDiscovery set

	interval := getPollingInterval(ds)
	if interval != 30*time.Second {
		t.Errorf("Expected default interval 30s, got %v", interval)
	}
}

func TestGetPollingInterval_Custom(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterDiscovery = &dittoiov1alpha1.AdapterDiscoverySpec{
		PollingInterval: "45s",
	}

	interval := getPollingInterval(ds)
	if interval != 45*time.Second {
		t.Errorf("Expected interval 45s, got %v", interval)
	}
}

func TestGetPollingInterval_Invalid(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterDiscovery = &dittoiov1alpha1.AdapterDiscoverySpec{
		PollingInterval: "invalid",
	}

	interval := getPollingInterval(ds)
	if interval != 30*time.Second {
		t.Errorf("Expected default interval 30s for invalid value, got %v", interval)
	}
}

func TestGetPollingInterval_NonPositive(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterDiscovery = &dittoiov1alpha1.AdapterDiscoverySpec{
		PollingInterval: "-5s",
	}

	interval := getPollingInterval(ds)
	if interval != 30*time.Second {
		t.Errorf("Expected default interval 30s for non-positive value, got %v", interval)
	}
}

func TestGetPollingInterval_Empty(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.AdapterDiscovery = &dittoiov1alpha1.AdapterDiscoverySpec{
		PollingInterval: "",
	}

	interval := getPollingInterval(ds)
	if interval != 30*time.Second {
		t.Errorf("Expected default interval 30s for empty value, got %v", interval)
	}
}

func TestMergeRequeueAfter(t *testing.T) {
	tests := []struct {
		name     string
		results  []ctrl.Result
		expected ctrl.Result
	}{
		{
			name:     "zero and 30s returns 30s",
			results:  []ctrl.Result{{}, {RequeueAfter: 30 * time.Second}},
			expected: ctrl.Result{RequeueAfter: 30 * time.Second},
		},
		{
			name:     "12m and 30s returns 30s",
			results:  []ctrl.Result{{RequeueAfter: 12 * time.Minute}, {RequeueAfter: 30 * time.Second}},
			expected: ctrl.Result{RequeueAfter: 30 * time.Second},
		},
		{
			name:     "30s and zero returns 30s",
			results:  []ctrl.Result{{RequeueAfter: 30 * time.Second}, {}},
			expected: ctrl.Result{RequeueAfter: 30 * time.Second},
		},
		{
			name:     "zero and zero returns zero",
			results:  []ctrl.Result{{}, {}},
			expected: ctrl.Result{},
		},
		{
			name:     "requeue true propagates",
			results:  []ctrl.Result{{Requeue: true}, {RequeueAfter: 30 * time.Second}},
			expected: ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second},
		},
		{
			name:     "single result",
			results:  []ctrl.Result{{RequeueAfter: 15 * time.Second}},
			expected: ctrl.Result{RequeueAfter: 15 * time.Second},
		},
		{
			name:     "three results picks minimum",
			results:  []ctrl.Result{{RequeueAfter: 60 * time.Second}, {RequeueAfter: 30 * time.Second}, {RequeueAfter: 45 * time.Second}},
			expected: ctrl.Result{RequeueAfter: 30 * time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeRequeueAfter(tt.results...)
			if result.RequeueAfter != tt.expected.RequeueAfter {
				t.Errorf("Expected RequeueAfter = %v, got %v", tt.expected.RequeueAfter, result.RequeueAfter)
			}
			if result.Requeue != tt.expected.Requeue {
				t.Errorf("Expected Requeue = %v, got %v", tt.expected.Requeue, result.Requeue)
			}
		})
	}
}
