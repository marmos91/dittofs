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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/conditions"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// authRetryAnnotation tracks the number of consecutive auth retries for backoff computation.
	authRetryAnnotation = "dittofs.dittofs.com/auth-retry-count"

	// maxBackoff is the maximum backoff duration for auth retries.
	maxBackoff = 5 * time.Minute

	// baseBackoff is the initial backoff duration for auth retries.
	baseBackoff = 2 * time.Second
)

// reconcileAdminCredentials ensures an admin credentials Secret exists for bootstrapping.
// The Secret contains the admin username and a random plaintext password that gets injected
// into the DittoFS pod as DITTOFS_ADMIN_INITIAL_PASSWORD env var.
func (r *DittoServerReconciler) reconcileAdminCredentials(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	// If user has explicitly provided admin password via passwordSecretRef, skip auto-generation
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.PasswordSecretRef != nil {
		return nil
	}

	secretName := dittoServer.GetAdminCredentialsSecretName()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: dittoServer.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, secret, r.Scheme); err != nil {
			return err
		}

		// Only generate password if Secret data is nil or empty
		if secret.Data == nil || len(secret.Data["password"]) == 0 {
			password, err := generateRandomPassword()
			if err != nil {
				return fmt.Errorf("failed to generate admin password: %w", err)
			}
			secret.Data = map[string][]byte{
				"username": []byte("admin"),
				"password": []byte(password),
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to create/update admin credentials secret: %w", err)
	}

	return nil
}

// reconcileAuth is the main auth reconciliation entry point.
// It provisions or refreshes the operator service account credentials.
func (r *DittoServerReconciler) reconcileAuth(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	apiURL := dittoServer.GetAPIServiceURL()

	// Check if operator credentials Secret exists
	credSecret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      dittoServer.GetOperatorCredentialsSecretName(),
	}, credSecret)

	var result ctrl.Result
	var authErr error

	if apierrors.IsNotFound(err) {
		// First-time provisioning
		result, authErr = r.provisionOperatorAccount(ctx, dittoServer, apiURL)
	} else if err != nil {
		authErr = fmt.Errorf("failed to get credentials secret: %w", err)
	} else {
		// Secret exists -- validate/refresh token
		result, authErr = r.refreshOperatorToken(ctx, dittoServer, credSecret, apiURL)
	}

	if authErr != nil {
		// Determine if this is a transient error (API unreachable)
		if isTransientError(authErr) {
			logger.Info("DittoFS API unreachable, will retry with backoff", "error", authErr.Error())

			retryCount := getRetryCount(dittoServer)

			// Emit K8s Event on first failure
			if retryCount == 0 {
				r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "AuthAPIUnreachable",
					"DittoFS API is unreachable: %v", authErr)
			}

			// Increment retry count
			if err := r.setRetryCount(ctx, dittoServer, retryCount+1); err != nil {
				logger.Error(err, "Failed to update retry count annotation")
			}

			// Set Authenticated condition to False
			r.setAuthCondition(ctx, dittoServer, metav1.ConditionFalse, "APIUnreachable",
				fmt.Sprintf("DittoFS API is unreachable: %v", authErr))

			backoff := computeBackoff(retryCount)
			return ctrl.Result{RequeueAfter: backoff}, nil
		}

		// Permanent failure
		r.setAuthCondition(ctx, dittoServer, metav1.ConditionFalse, "AuthenticationFailed",
			fmt.Sprintf("Authentication failed: %v", authErr))
		return ctrl.Result{}, authErr
	}

	// Success -- reset retry count and set condition
	if err := r.setRetryCount(ctx, dittoServer, 0); err != nil {
		logger.Error(err, "Failed to reset retry count annotation")
	}

	r.setAuthCondition(ctx, dittoServer, metav1.ConditionTrue, "AuthenticationSucceeded",
		"Operator service account authenticated successfully")

	return result, nil
}

// provisionOperatorAccount creates the operator service account and stores credentials.
func (r *DittoServerReconciler) provisionOperatorAccount(ctx context.Context, ds *dittoiov1alpha1.DittoServer, apiURL string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Provisioning operator service account", "apiURL", apiURL)

	// Read admin credentials
	adminSecret := &corev1.Secret{}
	adminSecretName := ds.GetAdminCredentialsSecretName()

	// If user provided admin password via spec, use that Secret
	if ds.Spec.Identity != nil && ds.Spec.Identity.Admin != nil &&
		ds.Spec.Identity.Admin.PasswordSecretRef != nil {
		ref := ds.Spec.Identity.Admin.PasswordSecretRef
		adminSecretName = ref.Name
	}

	if err := r.Get(ctx, client.ObjectKey{
		Namespace: ds.Namespace,
		Name:      adminSecretName,
	}, adminSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get admin credentials secret %s: %w", adminSecretName, err)
	}

	adminUsername := string(adminSecret.Data["username"])
	adminPassword := string(adminSecret.Data["password"])
	if adminUsername == "" {
		adminUsername = "admin"
	}

	// Login as admin
	apiClient := NewDittoFSClient(apiURL)
	tokenResp, err := apiClient.Login(ctx, adminUsername, adminPassword)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to login as admin: %w", err)
	}
	apiClient.SetToken(tokenResp.AccessToken)

	// Generate random password for operator service account
	operatorPassword, err := generateRandomPassword()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to generate operator password: %w", err)
	}

	// Create operator user
	err = apiClient.CreateUser(ctx, dittoiov1alpha1.OperatorServiceAccountUsername, operatorPassword, "operator")
	if err != nil {
		// If user already exists, proceed to login
		var apiErr *DittoFSAPIError
		if errors.As(err, &apiErr) && apiErr.IsConflict() {
			logger.Info("Operator service account already exists, proceeding to login")
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to create operator service account: %w", err)
		}
	}

	// Login as operator to get JWT tokens
	operatorTokens, err := apiClient.Login(ctx, dittoiov1alpha1.OperatorServiceAccountUsername, operatorPassword)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to login as operator: %w", err)
	}

	// Create operator credentials Secret
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.GetOperatorCredentialsSecretName(),
			Namespace: ds.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username":      []byte(dittoiov1alpha1.OperatorServiceAccountUsername),
			"password":      []byte(operatorPassword),
			"access-token":  []byte(operatorTokens.AccessToken),
			"refresh-token": []byte(operatorTokens.RefreshToken),
			"server-url":    []byte(apiURL),
		},
	}

	if err := controllerutil.SetControllerReference(ds, credSecret, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner reference on credentials secret: %w", err)
	}

	if err := r.Create(ctx, credSecret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Race condition: another reconcile already created it, update instead
			existing := &corev1.Secret{}
			if getErr := r.Get(ctx, client.ObjectKey{
				Namespace: ds.Namespace,
				Name:      ds.GetOperatorCredentialsSecretName(),
			}, existing); getErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get existing credentials secret: %w", getErr)
			}
			existing.Data = credSecret.Data
			if updateErr := r.Update(ctx, existing); updateErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update credentials secret: %w", updateErr)
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to create credentials secret: %w", err)
		}
	}

	logger.Info("Operator service account provisioned successfully")
	r.Recorder.Event(ds, corev1.EventTypeNormal, "AuthProvisioned",
		"Operator service account provisioned and credentials stored")

	// Schedule token refresh at 80% of TTL
	refreshInterval := operatorTokens.ExpiresInDuration() * 80 / 100
	if refreshInterval <= 0 {
		refreshInterval = 10 * time.Minute // Safe default
	}
	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

// refreshOperatorToken refreshes the operator's JWT token using the stored credentials.
func (r *DittoServerReconciler) refreshOperatorToken(ctx context.Context, ds *dittoiov1alpha1.DittoServer, secret *corev1.Secret, apiURL string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	refreshToken := string(secret.Data["refresh-token"])
	storedPassword := string(secret.Data["password"])

	apiClient := NewDittoFSClient(apiURL)

	// Try refresh first (cheapest operation)
	tokenResp, err := apiClient.RefreshToken(ctx, refreshToken)
	if err != nil {
		logger.Info("Token refresh failed, falling back to re-login", "error", err.Error())

		// Fallback: re-login with stored password
		tokenResp, err = apiClient.Login(ctx, dittoiov1alpha1.OperatorServiceAccountUsername, storedPassword)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("both token refresh and re-login failed: %w", err)
		}
	}

	// Update Secret with new tokens
	secret.Data["access-token"] = []byte(tokenResp.AccessToken)
	secret.Data["refresh-token"] = []byte(tokenResp.RefreshToken)
	if err := r.Update(ctx, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update credentials secret with new tokens: %w", err)
	}

	logger.V(1).Info("Operator token refreshed successfully")

	// Schedule next refresh at 80% of TTL
	refreshInterval := tokenResp.ExpiresInDuration() * 80 / 100
	if refreshInterval <= 0 {
		refreshInterval = 10 * time.Minute // Safe default
	}
	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

// cleanupOperatorServiceAccount attempts to delete the operator service account as best-effort cleanup.
// Returns nil even on failure -- this is cleanup during CR deletion and must not block finalizer removal.
func (r *DittoServerReconciler) cleanupOperatorServiceAccount(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
	logger := logf.FromContext(ctx)

	// Read admin credentials
	adminSecret := &corev1.Secret{}
	adminSecretName := ds.GetAdminCredentialsSecretName()

	if ds.Spec.Identity != nil && ds.Spec.Identity.Admin != nil &&
		ds.Spec.Identity.Admin.PasswordSecretRef != nil {
		adminSecretName = ds.Spec.Identity.Admin.PasswordSecretRef.Name
	}

	if err := r.Get(ctx, client.ObjectKey{
		Namespace: ds.Namespace,
		Name:      adminSecretName,
	}, adminSecret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("Admin credentials secret not found, skipping service account cleanup")
			return nil
		}
		logger.Info("Failed to get admin credentials for cleanup, skipping", "error", err.Error())
		return nil
	}

	adminUsername := string(adminSecret.Data["username"])
	adminPassword := string(adminSecret.Data["password"])
	if adminUsername == "" {
		adminUsername = "admin"
	}

	apiURL := ds.GetAPIServiceURL()
	apiClient := NewDittoFSClient(apiURL)

	// Login as admin
	tokenResp, err := apiClient.Login(ctx, adminUsername, adminPassword)
	if err != nil {
		logger.Info("Failed to login as admin for cleanup, skipping", "error", err.Error())
		return nil
	}
	apiClient.SetToken(tokenResp.AccessToken)

	// Delete operator user
	if err := apiClient.DeleteUser(ctx, dittoiov1alpha1.OperatorServiceAccountUsername); err != nil {
		logger.Info("Failed to delete operator service account (best-effort)", "error", err.Error())
		return nil
	}

	logger.Info("Operator service account deleted during cleanup")
	return nil
}

// setAuthCondition sets the Authenticated condition on the DittoServer status.
func (r *DittoServerReconciler) setAuthCondition(ctx context.Context, ds *dittoiov1alpha1.DittoServer, status metav1.ConditionStatus, reason, message string) {
	logger := logf.FromContext(ctx)

	// Re-fetch the latest version to avoid conflicts
	latest := &dittoiov1alpha1.DittoServer{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), latest); err != nil {
		logger.Error(err, "Failed to get latest DittoServer for condition update")
		return
	}

	conditions.SetCondition(&latest.Status.Conditions, latest.Generation,
		conditions.ConditionAuthenticated, status, reason, message)

	if err := r.Status().Update(ctx, latest); err != nil {
		logger.Error(err, "Failed to update Authenticated condition")
	}
}

// computeBackoff calculates exponential backoff duration for auth retries.
// Starts at 2s and doubles each retry, capped at 5 minutes.
func computeBackoff(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	// Cap the shift to avoid overflow
	if retryCount > 20 {
		return maxBackoff
	}
	backoff := baseBackoff * time.Duration(1<<retryCount)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	return backoff
}

// isTransientError returns true if the error indicates the API is temporarily unreachable.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	// Check for network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// Check for connection refused (wrapped in various ways)
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Check the error message for common transient patterns
	msg := err.Error()
	transientPatterns := []string{
		"connection refused",
		"no such host",
		"i/o timeout",
		"connection reset",
		"EOF",
	}
	for _, pattern := range transientPatterns {
		if containsIgnoreCase(msg, pattern) {
			return true
		}
	}

	// Check for DittoFS API errors that indicate transient issues
	var apiErr *DittoFSAPIError
	if errors.As(err, &apiErr) {
		// 502, 503, 504 are transient gateway errors
		// The API error code won't directly have status codes, but the message might
		return false // API errors mean the server is reachable, not transient
	}

	return false
}

// containsIgnoreCase checks if s contains substr (case-insensitive).
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// getRetryCount reads the auth retry count from the CR annotation.
func getRetryCount(ds *dittoiov1alpha1.DittoServer) int {
	if ds.Annotations == nil {
		return 0
	}
	val, ok := ds.Annotations[authRetryAnnotation]
	if !ok {
		return 0
	}
	count, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return count
}

// setRetryCount updates the auth retry count annotation on the CR.
func (r *DittoServerReconciler) setRetryCount(ctx context.Context, ds *dittoiov1alpha1.DittoServer, count int) error {
	// Re-fetch to avoid conflicts
	latest := &dittoiov1alpha1.DittoServer{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), latest); err != nil {
		return err
	}

	if latest.Annotations == nil {
		latest.Annotations = make(map[string]string)
	}

	if count == 0 {
		delete(latest.Annotations, authRetryAnnotation)
	} else {
		latest.Annotations[authRetryAnnotation] = strconv.Itoa(count)
	}

	return r.Update(ctx, latest)
}

// generateRandomPassword generates a cryptographically secure random password.
// Returns a 24-character URL-safe base64 string (18 bytes of randomness).
// This matches the pattern from models.GenerateRandomPassword().
func generateRandomPassword() (string, error) {
	b := make([]byte, 18)
	_, err := rand.Read(b)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
