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
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
)

// reconcileAuthenticatedState runs the reconciliation steps that require an
// authenticated DittoFS API: adapter discovery, per-adapter Services, network
// policies, and snapshot policies. Extracted from Reconcile to keep that
// method's cyclomatic complexity in check. Returns the adapter sub-reconciler's
// result (carrying any requeue cadence) and a non-nil error only for the
// security-critical NetworkPolicy step, which the caller propagates.
func (r *DittoServerReconciler) reconcileAuthenticatedState(ctx context.Context, ds *dittoiov1alpha1.DittoServer) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	adapterResult, _ := r.reconcileAdapters(ctx, ds)

	// Service reconciliation: sync adapter Services based on discovered state.
	if err := r.reconcileAdapterServices(ctx, ds); err != nil {
		logger.Error(err, "Failed to reconcile adapter services")
		r.Recorder.Eventf(ds, corev1.EventTypeWarning, "AdapterServiceFailed",
			"Failed to reconcile adapter services: %v", err)
		// Don't block reconciliation -- adapter services are best-effort.
	}

	// NetworkPolicy reconciliation: restrict ingress to active adapter ports.
	if err := r.reconcileNetworkPolicies(ctx, ds); err != nil {
		logger.Error(err, "Failed to reconcile adapter network policies")
		r.Recorder.Eventf(ds, corev1.EventTypeWarning, "NetworkPolicyFailed",
			"Failed to reconcile adapter network policies: %v", err)
		// NetworkPolicies are security-critical, propagate error.
		return ctrl.Result{}, err
	}

	// Snapshot policy reconciliation: push declared per-share policies.
	// Best-effort — a share that does not exist yet is skipped and retried on
	// the next reconcile; failures must not block the loop.
	if requeue := r.reconcileSnapshotPolicies(ctx, ds); requeue && adapterResult.RequeueAfter == 0 {
		adapterResult.RequeueAfter = 30 * time.Second
	}

	return adapterResult, nil
}

// reconcileSnapshotPolicies pushes each declared per-share snapshot policy to
// the DittoFS API. It is best-effort and idempotent (PUT upsert): a share that
// does not exist yet returns 404 and is skipped, signalling a requeue so the
// policy is retried once the share is provisioned. Other per-policy errors are
// logged + evented but never block the reconcile loop. The operator only
// upserts declared policies; it never deletes, so manually-created policies are
// preserved.
//
// Returns true when at least one policy could not be applied because its share
// was missing, so the caller can schedule a retry.
func (r *DittoServerReconciler) reconcileSnapshotPolicies(ctx context.Context, ds *dittoiov1alpha1.DittoServer) bool {
	logger := logf.FromContext(ctx)
	if len(ds.Spec.SnapshotPolicies) == 0 {
		return false
	}

	apiClient, err := r.getAuthenticatedClient(ctx, ds)
	if err != nil {
		logger.Info("Snapshot policy reconcile: no authenticated client, skipping", "error", err.Error())
		return false
	}

	retry := false
	for _, p := range ds.Spec.SnapshotPolicies {
		req := UpsertSnapshotPolicyRequest{
			Interval:   p.Interval,
			TTL:        p.TTL,
			Enabled:    p.Enabled,
			NamePrefix: p.NamePrefix,
		}
		if p.KeepLast != nil {
			req.KeepLast = int(*p.KeepLast)
		}

		if err := apiClient.UpsertSnapshotPolicy(ctx, p.Share, req); err != nil {
			var apiErr *DittoFSAPIError
			if errors.As(err, &apiErr) && apiErr.IsNotFound() {
				// Share not created yet — skip and retry on the next reconcile.
				logger.Info("Snapshot policy reconcile: share not found yet, will retry", "share", p.Share)
				retry = true
				continue
			}
			logger.Error(err, "Snapshot policy reconcile failed", "share", p.Share)
			r.Recorder.Eventf(ds, corev1.EventTypeWarning, "SnapshotPolicyFailed",
				"Failed to apply snapshot policy for share %s: %v", p.Share, err)
			continue
		}
		logger.V(1).Info("Snapshot policy applied", "share", p.Share)
	}
	return retry
}
