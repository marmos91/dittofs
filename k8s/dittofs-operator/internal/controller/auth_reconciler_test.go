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
	"net/http/httptest"
	"testing"
	"time"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/conditions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestDittoServer creates a DittoServer for auth reconciler tests.
func newTestDittoServer(name, namespace string) *dittoiov1alpha1.DittoServer {
	return dittoiov1alpha1.NewDittoServer(
		dittoiov1alpha1.WithName(name),
		dittoiov1alpha1.WithNamespace(namespace),
		dittoiov1alpha1.WithSpec(
			*dittoiov1alpha1.NewDittoServerSpec(
				dittoiov1alpha1.WithStorage(
					dittoiov1alpha1.StorageSpec{
						MetadataSize: "5Gi",
						CacheSize:    "5Gi",
					},
				),
			),
		),
	)
}

// setupAuthReconciler creates a reconciler with fake client and given objects.
func setupAuthReconciler(t *testing.T, objs ...runtime.Object) *DittoServerReconciler {
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := dittoiov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add v1alpha1 scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&dittoiov1alpha1.DittoServer{}).
		Build()

	return &DittoServerReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
	}
}

// mockDittoFSServer creates a mock DittoFS API server for testing.
// The handler function map lets tests customize behavior per endpoint.
func mockDittoFSServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	mux := http.NewServeMux()
	for pattern, handler := range handlers {
		mux.HandleFunc(pattern, handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// tokenJSON returns a JSON token response with the given access token.
func tokenJSON(accessToken string, expiresIn int64) []byte {
	resp := TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: "refresh-token-" + accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	data, _ := json.Marshal(resp)
	return data
}

func TestProvisionOperatorAccount_Success(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create admin credentials Secret
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetAdminCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("admin-pass"),
		},
	}

	r := setupAuthReconciler(t, ds, adminSecret)

	// Mock DittoFS API
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"POST /api/v1/auth/login": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(tokenJSON("test-access-token", 900))
		},
		"POST /api/v1/users": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"1","username":"k8s-operator","role":"operator"}`))
		},
	})

	// Override API URL by directly calling provisionOperatorAccount
	result, err := r.provisionOperatorAccount(context.Background(), ds, server.URL)
	if err != nil {
		t.Fatalf("provisionOperatorAccount failed: %v", err)
	}

	// Verify RequeueAfter is set for token refresh
	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0, got %v", result.RequeueAfter)
	}

	// Verify operator credentials Secret was created
	credSecret := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetOperatorCredentialsSecretName(),
	}, credSecret)
	if err != nil {
		t.Fatalf("Failed to get operator credentials secret: %v", err)
	}

	// Verify Secret has the expected keys
	expectedKeys := []string{"username", "password", "access-token", "refresh-token", "server-url"}
	for _, key := range expectedKeys {
		if _, ok := credSecret.Data[key]; !ok {
			t.Errorf("Operator credentials secret missing key: %s", key)
		}
	}

	// Verify username
	if string(credSecret.Data["username"]) != dittoiov1alpha1.OperatorServiceAccountUsername {
		t.Errorf("Expected username %s, got %s",
			dittoiov1alpha1.OperatorServiceAccountUsername, string(credSecret.Data["username"]))
	}

	// Verify access token
	if string(credSecret.Data["access-token"]) != "test-access-token" {
		t.Errorf("Expected access token 'test-access-token', got %s", string(credSecret.Data["access-token"]))
	}

	// Verify server URL
	if string(credSecret.Data["server-url"]) != server.URL {
		t.Errorf("Expected server URL %s, got %s", server.URL, string(credSecret.Data["server-url"]))
	}

	// Verify owner reference
	if len(credSecret.OwnerReferences) == 0 {
		t.Errorf("Operator credentials secret has no owner references")
	} else if credSecret.OwnerReferences[0].Kind != "DittoServer" {
		t.Errorf("Expected owner reference kind 'DittoServer', got %s", credSecret.OwnerReferences[0].Kind)
	}
}

func TestProvisionOperatorAccount_UserAlreadyExists(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetAdminCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("admin-pass"),
		},
	}

	r := setupAuthReconciler(t, ds, adminSecret)

	// Mock DittoFS API that returns 409 Conflict on CreateUser
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"POST /api/v1/auth/login": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(tokenJSON("operator-token", 900))
		},
		"POST /api/v1/users": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"code":"CONFLICT","message":"User already exists"}`))
		},
	})

	result, err := r.provisionOperatorAccount(context.Background(), ds, server.URL)
	if err != nil {
		t.Fatalf("provisionOperatorAccount should succeed when user already exists, got: %v", err)
	}

	// Verify it still created the credentials Secret
	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0, got %v", result.RequeueAfter)
	}

	credSecret := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetOperatorCredentialsSecretName(),
	}, credSecret)
	if err != nil {
		t.Fatalf("Failed to get operator credentials secret: %v", err)
	}

	if string(credSecret.Data["access-token"]) != "operator-token" {
		t.Errorf("Expected access token 'operator-token', got %s", string(credSecret.Data["access-token"]))
	}
}

func TestRefreshOperatorToken_RefreshSuccess(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	// Create existing credentials Secret
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username":      []byte("k8s-operator"),
			"password":      []byte("stored-password"),
			"access-token":  []byte("old-access-token"),
			"refresh-token": []byte("old-refresh-token"),
			"server-url":    []byte("http://old-url"),
		},
	}

	r := setupAuthReconciler(t, ds, credSecret)

	// Mock DittoFS API that accepts refresh
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"POST /api/v1/auth/refresh": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(tokenJSON("new-access-token", 900))
		},
	})

	result, err := r.refreshOperatorToken(context.Background(), ds, credSecret, server.URL)
	if err != nil {
		t.Fatalf("refreshOperatorToken failed: %v", err)
	}

	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0 for next refresh, got %v", result.RequeueAfter)
	}

	// Re-fetch the secret to verify update
	updated := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetOperatorCredentialsSecretName(),
	}, updated)
	if err != nil {
		t.Fatalf("Failed to get updated credentials secret: %v", err)
	}

	if string(updated.Data["access-token"]) != "new-access-token" {
		t.Errorf("Expected updated access token 'new-access-token', got %s", string(updated.Data["access-token"]))
	}
}

func TestRefreshOperatorToken_RefreshFails_ReloginSuccess(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username":      []byte("k8s-operator"),
			"password":      []byte("stored-password"),
			"access-token":  []byte("old-access-token"),
			"refresh-token": []byte("expired-refresh-token"),
			"server-url":    []byte("http://old-url"),
		},
	}

	r := setupAuthReconciler(t, ds, credSecret)

	// Mock DittoFS API: refresh fails, but login succeeds
	server := mockDittoFSServer(t, map[string]http.HandlerFunc{
		"POST /api/v1/auth/refresh": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"UNAUTHORIZED","message":"Invalid refresh token"}`))
		},
		"POST /api/v1/auth/login": func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(tokenJSON("relogin-access-token", 900))
		},
	})

	result, err := r.refreshOperatorToken(context.Background(), ds, credSecret, server.URL)
	if err != nil {
		t.Fatalf("refreshOperatorToken should succeed with re-login fallback, got: %v", err)
	}

	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0, got %v", result.RequeueAfter)
	}

	// Verify the token was updated
	updated := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetOperatorCredentialsSecretName(),
	}, updated)
	if err != nil {
		t.Fatalf("Failed to get updated credentials secret: %v", err)
	}

	if string(updated.Data["access-token"]) != "relogin-access-token" {
		t.Errorf("Expected 'relogin-access-token', got %s", string(updated.Data["access-token"]))
	}
}

func TestReconcileAuth_APIUnreachable(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetAdminCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("admin-pass"),
		},
	}

	r := setupAuthReconciler(t, ds, adminSecret)

	// Use a non-existent URL to simulate API unreachable
	// Override the API URL by temporarily modifying the DittoServer's control plane config
	// to point to a closed port
	ds.Spec.ControlPlane = &dittoiov1alpha1.ControlPlaneAPIConfig{Port: 1} // unlikely to be open

	result, err := r.reconcileAuth(context.Background(), ds)

	// Should not return an error (transient errors are handled with backoff)
	if err != nil {
		t.Fatalf("reconcileAuth should not return error for transient failures, got: %v", err)
	}

	// Should request requeue with backoff
	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0 for backoff, got %v", result.RequeueAfter)
	}

	// Verify Authenticated condition is False
	updatedDS := &dittoiov1alpha1.DittoServer{}
	err = r.Get(context.Background(), client.ObjectKeyFromObject(ds), updatedDS)
	if err != nil {
		t.Fatalf("Failed to get updated DittoServer: %v", err)
	}

	authCond := conditions.GetCondition(updatedDS.Status.Conditions, conditions.ConditionAuthenticated)
	if authCond == nil {
		t.Fatalf("Authenticated condition not found")
	}
	if authCond.Status != metav1.ConditionFalse {
		t.Errorf("Expected Authenticated=False, got %s", authCond.Status)
	}
	if authCond.Reason != "APIUnreachable" {
		t.Errorf("Expected reason 'APIUnreachable', got %s", authCond.Reason)
	}

	// Verify no K8s resources were deleted (admin Secret still exists)
	stillExists := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetAdminCredentialsSecretName(),
	}, stillExists)
	if err != nil {
		t.Errorf("Admin credentials Secret should still exist after transient failure: %v", err)
	}
}

func TestCleanupOperatorServiceAccount_BestEffort(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetAdminCredentialsSecretName(),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("admin-pass"),
		},
	}

	r := setupAuthReconciler(t, ds, adminSecret)

	// The API is unreachable (no mock server), cleanup should still return nil
	err := r.cleanupOperatorServiceAccount(context.Background(), ds)
	if err != nil {
		t.Errorf("cleanupOperatorServiceAccount should return nil on failure (best-effort), got: %v", err)
	}
}

func TestReconcileAdminCredentials_AutoGenerate(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")

	r := setupAuthReconciler(t, ds)

	// First call: should create the Secret
	err := r.reconcileAdminCredentials(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdminCredentials failed: %v", err)
	}

	// Verify Secret was created
	secret := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetAdminCredentialsSecretName(),
	}, secret)
	if err != nil {
		t.Fatalf("Failed to get admin credentials secret: %v", err)
	}

	if string(secret.Data["username"]) != "admin" {
		t.Errorf("Expected username 'admin', got %s", string(secret.Data["username"]))
	}

	password := string(secret.Data["password"])
	if len(password) == 0 {
		t.Errorf("Expected non-empty password")
	}

	// Verify idempotency: calling again should not change the password
	err = r.reconcileAdminCredentials(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdminCredentials (second call) failed: %v", err)
	}

	secret2 := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetAdminCredentialsSecretName(),
	}, secret2)
	if err != nil {
		t.Fatalf("Failed to get admin credentials secret (second call): %v", err)
	}

	if string(secret2.Data["password"]) != password {
		t.Errorf("Password changed on second call (should be idempotent)")
	}
}

func TestReconcileAdminCredentials_SkipWhenUserProvided(t *testing.T) {
	ds := newTestDittoServer("test-server", "default")
	ds.Spec.Identity = &dittoiov1alpha1.IdentityConfig{
		Admin: &dittoiov1alpha1.AdminConfig{
			PasswordSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "user-provided-secret"},
				Key:                  "password-hash",
			},
		},
	}

	r := setupAuthReconciler(t, ds)

	// Should skip auto-generation
	err := r.reconcileAdminCredentials(context.Background(), ds)
	if err != nil {
		t.Fatalf("reconcileAdminCredentials should skip when user provides secret: %v", err)
	}

	// Verify no Secret was created
	secret := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      ds.GetAdminCredentialsSecretName(),
	}, secret)
	if err == nil {
		t.Errorf("Admin credentials Secret should NOT be created when user provides passwordSecretRef")
	}
}

func TestComputeBackoff(t *testing.T) {
	tests := []struct {
		retryCount int
		minBackoff time.Duration
		maxBackoff time.Duration
	}{
		{0, 2 * time.Second, 2 * time.Second},
		{1, 4 * time.Second, 4 * time.Second},
		{2, 8 * time.Second, 8 * time.Second},
		{3, 16 * time.Second, 16 * time.Second},
		{7, 256 * time.Second, 256 * time.Second},
		{8, 5 * time.Minute, 5 * time.Minute}, // Capped
		{20, 5 * time.Minute, 5 * time.Minute},
		{100, 5 * time.Minute, 5 * time.Minute},
	}

	for _, tt := range tests {
		result := computeBackoff(tt.retryCount)
		if result < tt.minBackoff || result > tt.maxBackoff {
			t.Errorf("computeBackoff(%d) = %v, want between %v and %v",
				tt.retryCount, result, tt.minBackoff, tt.maxBackoff)
		}
	}
}

func TestComputeBackoff_NegativeRetry(t *testing.T) {
	result := computeBackoff(-1)
	if result != 2*time.Second {
		t.Errorf("computeBackoff(-1) = %v, want %v", result, 2*time.Second)
	}
}
