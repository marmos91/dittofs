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
	"fmt"
	"strings"
	"sync"
	"time"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/internal/controller/config"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/percona"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/resources"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/conditions"
	pgv2 "github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// finalizerName is the finalizer for DittoServer cleanup
	finalizerName = "dittofs.dittofs.com/finalizer"
	// cleanupTimeout is the maximum time to wait for cleanup before force-removing finalizer
	cleanupTimeout = 60 * time.Second
	// maxRetries is the number of retries for CreateOrUpdate operations on conflict
	maxRetries = 3
	// retryBackoffBase is the base duration for exponential backoff on retries
	retryBackoffBase = 100 * time.Millisecond
	// defaultAPIPort is the default control plane API port
	defaultAPIPort = 8080
	// defaultFSGroup is the default fsGroup for pod security context (nonroot user)
	defaultFSGroup = 65532
	// capabilityAll is the special capability value that drops all Linux
	// capabilities from the managed dfs container.
	capabilityAll corev1.Capability = "ALL"
)

// retryOnConflict wraps an operation with retry logic for optimistic locking conflicts.
// This is necessary because CreateOrUpdate can race with status updates from other controllers.
//
// Retries are immediate (no sleep): a burst of back-to-back CAS conflicts within
// the same goroutine scheduling quantum is resolved cheaply, but the goroutine is
// never blocked. If all attempts conflict, the conflict error is returned and the
// caller must convert it via conflictResult so the work queue reschedules the
// reconcile rather than the goroutine busy-waiting.
func retryOnConflict(fn func() error) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

// conflictResult converts a persistent optimistic-lock conflict into a
// requeue-after result so the reconcile goroutine is freed immediately and the
// work queue reschedules after retryBackoffBase. Non-conflict errors are
// returned unchanged so the work queue's rate limiter applies exponential
// back-off.
func conflictResult(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: retryBackoffBase}, nil
	}
	return ctrl.Result{}, err
}

// stepError converts a failed reconcile step into a Reconcile result: a
// transient optimistic-lock conflict is requeued (freeing the goroutine
// immediately), anything else is recorded as a warning event, logged, and
// returned. Centralizing this keeps Reconcile's per-step error handling uniform.
func (r *DittoServerReconciler) stepError(ctx context.Context, ds *dittoiov1alpha1.DittoServer, what string, err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: retryBackoffBase}, nil
	}
	r.Recorder.Eventf(ds, corev1.EventTypeWarning, "ReconcileFailed", "Failed to reconcile %s: %v", what, err)
	logf.FromContext(ctx).Error(err, "Failed to reconcile "+what)
	return ctrl.Result{}, err
}

// DittoServerReconciler reconciles a DittoServer object
type DittoServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// adaptersMu protects lastKnownAdapters for concurrent reconcile safety.
	adaptersMu sync.RWMutex
	// lastKnownAdapters stores the last successful adapter poll result per CR.
	// Key is namespace/name of the DittoServer CR.
	lastKnownAdapters map[string][]AdapterInfo
}

// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgv2.percona.com,resources=perconapgclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgv2.percona.com,resources=perconapgclusters/status,verbs=get
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the cluster state matches the desired DittoServer spec.
func (r *DittoServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	dittoServer := &dittoiov1alpha1.DittoServer{}
	if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !dittoServer.DeletionTimestamp.IsZero() {
		requeue, err := r.handleDeletion(ctx, dittoServer)
		if err != nil {
			// A persistent finalizer-removal conflict is requeued (not surfaced
			// as an error) so the goroutine is freed immediately.
			return conflictResult(err)
		}
		if requeue {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(dittoServer, finalizerName) {
		logger.Info("Adding finalizer to DittoServer")
		controllerutil.AddFinalizer(dittoServer, finalizerName)
		if err := r.Update(ctx, dittoServer); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(dittoServer, corev1.EventTypeNormal, "Created",
			"DittoServer created, finalizer added")
		// Requeue to continue with reconciliation after finalizer is added
		return ctrl.Result{Requeue: true}, nil
	}

	// Ensure JWT secret exists (auto-generate if not provided)
	if err := r.reconcileJWTSecret(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile JWT Secret: %v", err)
		logger.Error(err, "Failed to reconcile JWT Secret")
		return ctrl.Result{}, err
	}

	// Ensure admin credentials Secret exists (auto-generate if not provided)
	// This must happen before ConfigMap so the Secret exists when the StatefulSet is created.
	if err := r.reconcileAdminCredentials(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile admin credentials: %v", err)
		logger.Error(err, "Failed to reconcile admin credentials")
		return ctrl.Result{}, err
	}

	if err := r.reconcileConfigMap(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile ConfigMap: %v", err)
		logger.Error(err, "Failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	// Reconcile services (headless required for StatefulSet DNS)
	if err := r.reconcileHeadlessService(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile headless Service: %v", err)
		logger.Error(err, "Failed to reconcile headless Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileAPIService(ctx, dittoServer); err != nil {
		return r.stepError(ctx, dittoServer, "API Service", err)
	}

	// Reconcile the metrics integration (metrics Service + optional, CRD-gated
	// ServiceMonitor). No-op when metrics are disabled; never fails the
	// reconcile merely because the prometheus-operator CRDs are absent.
	if err := r.reconcileMetrics(ctx, dittoServer); err != nil {
		return r.stepError(ctx, dittoServer, "Metrics", err)
	}

	// Reconcile PerconaPGCluster if Percona is enabled
	if err := r.reconcilePerconaPGCluster(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile PerconaPGCluster: %v", err)
		logger.Error(err, "Failed to reconcile PerconaPGCluster")
		return ctrl.Result{}, err
	}

	// Check if Percona is enabled but not ready - block StatefulSet creation
	if isPerconaEnabled(dittoServer) {
		pgCluster := &pgv2.PerconaPGCluster{}
		err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      percona.ClusterName(dittoServer.Name),
		}, pgCluster)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("Waiting for PerconaPGCluster to be created")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		if !percona.IsReady(pgCluster) {
			r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "PerconaNotReady",
				"Waiting for PostgreSQL cluster %s (state: %s)", percona.ClusterName(dittoServer.Name), percona.GetState(pgCluster))
			logger.Info("Waiting for PerconaPGCluster to be ready", "state", percona.GetState(pgCluster))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	replicas := int32(1)
	if dittoServer.Spec.Replicas != nil {
		replicas = *dittoServer.Spec.Replicas
	}

	configHash, err := r.reconcileStatefulSet(ctx, dittoServer, replicas)
	if err != nil {
		return r.stepError(ctx, dittoServer, "StatefulSet", err)
	}

	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      dittoServer.Name,
	}, statefulSet); err != nil {
		logger.Error(err, "Failed to get StatefulSet")
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, dittoServer, statefulSet, replicas, configHash); err != nil {
		logger.Error(err, "Failed to update DittoServer status")
		return ctrl.Result{}, err
	}

	// Ensure baseline NetworkPolicy allowing API traffic exists before auth.
	// This prevents adapter NetworkPolicies from blocking operator-to-API communication.
	if err := r.ensureBaselineNetworkPolicy(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to ensure baseline network policy")
		return ctrl.Result{}, err
	}

	// Auth reconciliation: only when StatefulSet has at least one ready replica
	if statefulSet.Status.ReadyReplicas >= 1 {
		authResult, authErr := r.reconcileAuth(ctx, dittoServer)
		if authErr != nil {
			logger.Error(authErr, "Auth reconciliation failed")
			r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "AuthFailed",
				"Failed to authenticate with DittoFS API: %v", authErr)
			// Don't return error -- auth failure should not block infrastructure reconciliation
			// The Authenticated condition will reflect the failure
		}

		// Re-fetch to get updated conditions after auth reconciliation
		if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		// Adapter discovery + dependent reconciliation: only if authenticated.
		var adapterResult ctrl.Result
		if conditions.IsConditionTrue(dittoServer.Status.Conditions, conditions.ConditionAuthenticated) {
			var rerr error
			adapterResult, rerr = r.reconcileAuthenticatedState(ctx, dittoServer)
			if rerr != nil {
				return ctrl.Result{}, rerr
			}
		}

		// Use minimum RequeueAfter from all sub-reconcilers
		result := mergeRequeueAfter(authResult, adapterResult)
		if result.RequeueAfter > 0 || result.Requeue {
			return result, nil
		}
	}

	return ctrl.Result{}, nil
}

// mergeRequeueAfter returns a Result with the minimum positive RequeueAfter
// from the given results. This ensures the fastest-cycling sub-reconciler
// drives the reconcile cadence.
func mergeRequeueAfter(results ...ctrl.Result) ctrl.Result {
	var minResult ctrl.Result
	for _, r := range results {
		if r.Requeue {
			minResult.Requeue = true
		}
		if r.RequeueAfter > 0 {
			if minResult.RequeueAfter == 0 || r.RequeueAfter < minResult.RequeueAfter {
				minResult.RequeueAfter = r.RequeueAfter
			}
		}
	}
	return minResult
}

// updateStatus computes and persists the DittoServer status from current cluster state.
func (r *DittoServerReconciler) updateStatus(
	ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer,
	statefulSet *appsv1.StatefulSet, replicas int32, configHash string,
) error {
	dittoServerCopy := dittoServer.DeepCopy()
	dittoServerCopy.Status.ObservedGeneration = dittoServer.Generation
	dittoServerCopy.Status.Replicas = replicas
	dittoServerCopy.Status.ReadyReplicas = statefulSet.Status.ReadyReplicas
	dittoServerCopy.Status.AvailableReplicas = statefulSet.Status.AvailableReplicas
	dittoServerCopy.Status.ConfigHash = configHash

	// Set PerconaClusterName if Percona is enabled
	if isPerconaEnabled(dittoServer) {
		dittoServerCopy.Status.PerconaClusterName = percona.ClusterName(dittoServer.Name)
	}

	dittoServerCopy.Status.Phase = determinePhase(replicas, statefulSet.Status.ReadyReplicas)

	// Set ConfigReady condition
	r.updateConfigReadyCondition(ctx, dittoServer, &dittoServerCopy.Status)

	// Set DatabaseReady condition (only relevant when Percona enabled)
	r.updateDatabaseReadyCondition(ctx, dittoServer, &dittoServerCopy.Status)

	// Set Available condition
	updateAvailableCondition(dittoServer, &dittoServerCopy.Status, replicas, statefulSet)

	// Set Progressing condition
	updateProgressingCondition(dittoServer, &dittoServerCopy.Status, replicas, statefulSet)

	// Set Ready condition (aggregate of other conditions)
	updateReadyCondition(dittoServer, &dittoServerCopy.Status, replicas)

	return r.Status().Update(ctx, dittoServerCopy)
}

// updateDatabaseReadyCondition sets the DatabaseReady condition based on Percona state.
func (r *DittoServerReconciler) updateDatabaseReadyCondition(
	ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer,
	status *dittoiov1alpha1.DittoServerStatus,
) {
	if !isPerconaEnabled(dittoServer) {
		conditions.RemoveCondition(&status.Conditions, conditions.ConditionDatabaseReady)
		return
	}

	pgCluster := &pgv2.PerconaPGCluster{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      percona.ClusterName(dittoServer.Name),
	}, pgCluster)
	if err != nil {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionDatabaseReady, metav1.ConditionFalse, "PerconaNotFound",
			fmt.Sprintf("PerconaPGCluster not found: %v", err))
	} else if !percona.IsReady(pgCluster) {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionDatabaseReady, metav1.ConditionFalse, "PerconaNotReady",
			fmt.Sprintf("PostgreSQL cluster state: %s", percona.GetState(pgCluster)))
	} else {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionDatabaseReady, metav1.ConditionTrue, "PerconaReady",
			"PostgreSQL cluster is ready")
	}
}

// updateAvailableCondition sets the Available condition based on replica readiness.
func updateAvailableCondition(
	dittoServer *dittoiov1alpha1.DittoServer,
	status *dittoiov1alpha1.DittoServerStatus,
	replicas int32, statefulSet *appsv1.StatefulSet,
) {
	if replicas == 0 {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionAvailable, metav1.ConditionTrue, "Stopped",
			"DittoServer is stopped (replicas=0)")
	} else if statefulSet.Status.ReadyReplicas >= 1 {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionAvailable, metav1.ConditionTrue, "MinimumReplicasAvailable",
			fmt.Sprintf("StatefulSet has %d/%d ready replicas", statefulSet.Status.ReadyReplicas, replicas))
	} else {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionAvailable, metav1.ConditionFalse, "NoReplicasAvailable",
			fmt.Sprintf("Waiting for replicas: %d/%d ready", statefulSet.Status.ReadyReplicas, replicas))
	}
}

// updateProgressingCondition sets the Progressing condition based on StatefulSet state.
func updateProgressingCondition(
	dittoServer *dittoiov1alpha1.DittoServer,
	status *dittoiov1alpha1.DittoServerStatus,
	replicas int32, statefulSet *appsv1.StatefulSet,
) {
	if statefulSet.Status.ObservedGeneration < statefulSet.Generation {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionProgressing, metav1.ConditionTrue, "StatefulSetUpdating",
			"StatefulSet is being updated")
	} else if statefulSet.Status.ReadyReplicas != replicas {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionProgressing, metav1.ConditionTrue, "ScalingReplicas",
			fmt.Sprintf("Scaling: %d/%d replicas ready", statefulSet.Status.ReadyReplicas, replicas))
	} else {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionProgressing, metav1.ConditionFalse, "ReconcileComplete",
			"All resources are up to date")
	}
}

// updateReadyCondition sets the aggregate Ready condition from other conditions.
func updateReadyCondition(
	dittoServer *dittoiov1alpha1.DittoServer,
	status *dittoiov1alpha1.DittoServerStatus,
	replicas int32,
) {
	configReady := conditions.IsConditionTrue(status.Conditions, conditions.ConditionConfigReady)
	available := conditions.IsConditionTrue(status.Conditions, conditions.ConditionAvailable)
	progressing := conditions.IsConditionTrue(status.Conditions, conditions.ConditionProgressing)

	databaseReady := true
	if isPerconaEnabled(dittoServer) {
		databaseReady = conditions.IsConditionTrue(status.Conditions, conditions.ConditionDatabaseReady)
	}

	// Authenticated is only required when the server is running (replicas > 0).
	// When stopped (replicas=0), there's no DittoFS API to authenticate against.
	authenticated := true
	if replicas > 0 {
		authenticated = conditions.IsConditionTrue(status.Conditions, conditions.ConditionAuthenticated)
	}

	allReady := configReady && available && !progressing && databaseReady && authenticated
	if allReady {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionReady, metav1.ConditionTrue, "AllConditionsMet",
			"DittoServer is fully operational")
	} else {
		reasons := collectNotReadyReasons(configReady, available, progressing, databaseReady, authenticated)
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionReady, metav1.ConditionFalse, "ConditionsNotMet",
			fmt.Sprintf("Not ready: %v", reasons))
	}
}

// handleDeletion processes DittoServer deletion, performing cleanup before allowing deletion to proceed.
// Returns (requeue, error) - if requeue is true, reconciliation should be requeued.
func (r *DittoServerReconciler) handleDeletion(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) (bool, error) {
	logger := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(dittoServer, finalizerName) {
		// Finalizer already removed, nothing to do
		return false, nil
	}

	logger.Info("Processing DittoServer deletion", "name", dittoServer.Name)

	// Update phase to Deleting (only once - re-writing on every reconcile bumps
	// resourceVersion needlessly and is wasteful). The status write must not be
	// allowed to poison the in-memory object used for the finalizer-removal Update
	// below, so it operates on a copy and any RV refresh is discarded.
	if dittoServer.Status.Phase != "Deleting" {
		dittoServerCopy := dittoServer.DeepCopy()
		dittoServerCopy.Status.Phase = "Deleting"
		if err := r.Status().Update(ctx, dittoServerCopy); err != nil {
			logger.Error(err, "Failed to update phase to Deleting")
			// Continue with cleanup even if status update fails
		}
	}
	r.Recorder.Event(dittoServer, corev1.EventTypeNormal, "Deleting",
		"DittoServer is being deleted, cleaning up resources")

	// Check how long we've been trying to delete
	deletionTime := dittoServer.DeletionTimestamp.Time
	elapsed := time.Since(deletionTime)

	if elapsed > cleanupTimeout {
		logger.Info("Cleanup timeout exceeded, forcing finalizer removal",
			"elapsed", elapsed, "timeout", cleanupTimeout)
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "CleanupTimeout",
			"Cleanup timeout exceeded (%v), forcing finalizer removal", cleanupTimeout)
		// Force remove finalizer after timeout
		if err := r.removeFinalizer(ctx, dittoServer); err != nil {
			return false, err
		}
		return false, nil
	}

	// Perform cleanup
	if err := r.performCleanup(ctx, dittoServer); err != nil {
		logger.Error(err, "Cleanup failed, will retry")
		// Requeue with backoff
		return true, nil
	}

	// Cleanup successful, remove finalizer
	logger.Info("Cleanup complete, removing finalizer")
	if err := r.removeFinalizer(ctx, dittoServer); err != nil {
		return false, err
	}

	return false, nil
}

// removeFinalizer removes the DittoServer finalizer, re-fetching the object on
// each attempt so the Update never carries a stale resourceVersion (e.g. one
// invalidated by the earlier Phase=Deleting status write). Wrapping in
// retryOnConflict makes it resilient to concurrent writers.
func (r *DittoServerReconciler) removeFinalizer(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	key := client.ObjectKeyFromObject(dittoServer)
	return retryOnConflict(func() error {
		current := &dittoiov1alpha1.DittoServer{}
		if err := r.Get(ctx, key, current); err != nil {
			// Object already gone: finalizer effectively removed.
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.ContainsFinalizer(current, finalizerName) {
			return nil
		}
		controllerutil.RemoveFinalizer(current, finalizerName)
		return r.Update(ctx, current)
	})
}

// performCleanup handles cleanup of resources that need special handling beyond owner references.
// Owned resources (StatefulSet, Services, ConfigMap) are automatically garbage collected.
// This handles Percona orphaning/deletion based on spec.percona.deleteWithServer.
func (r *DittoServerReconciler) performCleanup(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	logger := logf.FromContext(ctx)

	// Handle Percona cleanup based on deleteWithServer flag
	if isPerconaEnabled(dittoServer) {
		clusterName := percona.ClusterName(dittoServer.Name)
		pgCluster := &pgv2.PerconaPGCluster{}
		err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      clusterName,
		}, pgCluster)

		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get PerconaPGCluster: %w", err)
		}

		if err == nil {
			// PerconaPGCluster exists
			if dittoServer.Spec.Percona.DeleteWithServer {
				// Delete the PerconaPGCluster - it will cascade to PVCs
				logger.Info("Deleting PerconaPGCluster (deleteWithServer=true)",
					"name", clusterName)
				if err := r.Delete(ctx, pgCluster); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("failed to delete PerconaPGCluster: %w", err)
				}
				r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "PerconaDeleted",
					"PerconaPGCluster %s deleted (deleteWithServer=true)", clusterName)
				// Note: Deletion is async, but we proceed since owner reference is being removed
			} else {
				// Orphan the PerconaPGCluster by removing our owner reference
				logger.Info("Orphaning PerconaPGCluster (deleteWithServer=false)",
					"name", clusterName)

				// Remove owner reference
				var newOwnerRefs []metav1.OwnerReference
				for _, ref := range pgCluster.OwnerReferences {
					if ref.UID != dittoServer.UID {
						newOwnerRefs = append(newOwnerRefs, ref)
					}
				}

				if len(newOwnerRefs) != len(pgCluster.OwnerReferences) {
					pgCluster.OwnerReferences = newOwnerRefs
					if err := r.Update(ctx, pgCluster); err != nil {
						return fmt.Errorf("failed to orphan PerconaPGCluster: %w", err)
					}
					r.Recorder.Eventf(dittoServer, corev1.EventTypeNormal, "PerconaOrphaned",
						"PerconaPGCluster %s orphaned and will be preserved", clusterName)
					logger.Info("PerconaPGCluster orphaned successfully", "name", clusterName)
				}
			}
		}
	}

	// Best-effort: delete DittoFS operator service account
	if err := r.cleanupOperatorServiceAccount(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to delete operator service account (best-effort)")
		// Don't return error -- best-effort cleanup
	}

	// Other owned resources (StatefulSet, Services, ConfigMap) are automatically
	// garbage collected via owner references when DittoServer is deleted.
	// No additional cleanup needed for them.

	return nil
}

// updateConfigReadyCondition checks ConfigMap and sets ConfigReady condition.
func (r *DittoServerReconciler) updateConfigReadyCondition(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer, statusCopy *dittoiov1alpha1.DittoServerStatus) {
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      dittoServer.Name + "-config",
	}, configMap)

	if err != nil {
		conditions.SetCondition(&statusCopy.Conditions, dittoServer.Generation,
			conditions.ConditionConfigReady, metav1.ConditionFalse, "ConfigMapNotFound",
			fmt.Sprintf("ConfigMap %s-config not found: %v", dittoServer.Name, err))
		return
	}

	if _, ok := configMap.Data["config.yaml"]; !ok {
		conditions.SetCondition(&statusCopy.Conditions, dittoServer.Generation,
			conditions.ConditionConfigReady, metav1.ConditionFalse, "ConfigMissing",
			"ConfigMap does not contain config.yaml key")
		return
	}

	conditions.SetCondition(&statusCopy.Conditions, dittoServer.Generation,
		conditions.ConditionConfigReady, metav1.ConditionTrue, "ConfigValid",
		"ConfigMap is valid and contains configuration")
}

// SetupWithManager sets up the controller with the Manager.
func (r *DittoServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&dittoiov1alpha1.DittoServer{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("dittoserver")

	// Conditionally watch PerconaPGCluster only if CRD exists
	// This allows the operator to work without Percona Operator installed
	_, err := mgr.GetRESTMapper().RESTMapping(pgv2.GroupVersion.WithKind("PerconaPGCluster").GroupKind())
	if err == nil {
		builder = builder.Owns(&pgv2.PerconaPGCluster{})
	}

	// Conditionally own ServiceMonitor only if the prometheus-operator CRDs are
	// installed, so the controller self-heals an externally deleted monitor. On
	// clusters without the CRDs this watch is skipped (no hard dependency).
	if _, err := mgr.GetRESTMapper().RESTMapping(serviceMonitorGVK.GroupKind(), serviceMonitorGVK.Version); err == nil {
		sm := &unstructured.Unstructured{}
		sm.SetGroupVersionKind(serviceMonitorGVK)
		builder = builder.Owns(sm)
	}

	return builder.Complete(r)
}

// reconcileJWTSecret ensures a JWT signing secret exists for the DittoServer.
// If the user hasn't provided a JWT secretRef, we auto-generate one and store it
// in a managed Kubernetes Secret.
func (r *DittoServerReconciler) reconcileJWTSecret(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	// If user has explicitly provided a JWT secret reference, use that
	if dittoServer.HasUserProvidedJWTSecret() {
		return nil
	}

	secretName := dittoServer.GetManagedJWTSecretName()
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

		// Only generate secret if it doesn't already exist
		if secret.Data == nil || len(secret.Data[dittoiov1alpha1.ManagedJWTSecretKey]) == 0 {
			// Generate a 32-byte random secret (64 hex chars)
			jwtSecret, err := generateRandomSecret(32)
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{
				dittoiov1alpha1.ManagedJWTSecretKey: []byte(jwtSecret),
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to create/update JWT secret: %w", err)
	}

	// Note: We don't mutate dittoServer.Spec here. Instead, use GetEffectiveJWTSecretRef()
	// to get the correct secret reference when needed.
	return nil
}

// generateRandomSecret generates a cryptographically secure random string.
// nBytes raw bytes are read from crypto/rand and returned as a base64
// RawURL-encoded string (no padding, URL-safe alphabet). The output length is
// ceil(nBytes * 4 / 3). The base64 alphabet has 64 symbols (256 % 64 == 0), so
// there is no modular bias. Returns an error if random generation fails (should
// never happen with crypto/rand).
func generateRandomSecret(nBytes int) (string, error) { //nolint:unparam // byte length kept as a parameter for the test suite's length/bias coverage
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (r *DittoServerReconciler) reconcileConfigMap(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dittoServer.Name + "-config",
			Namespace: dittoServer.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, configMap, r.Scheme); err != nil {
			return err
		}

		configYAML, err := config.GenerateDittoFSConfig(dittoServer)
		if err != nil {
			return fmt.Errorf("failed to generate config: %w", err)
		}

		configMap.Data = map[string]string{
			"config.yaml": configYAML,
		}

		return nil
	})

	return err
}

// getServiceType returns the Kubernetes Service type from the CRD spec.
func getServiceType(dittoServer *dittoiov1alpha1.DittoServer) corev1.ServiceType {
	if dittoServer.Spec.Service.Type != "" {
		return corev1.ServiceType(dittoServer.Spec.Service.Type)
	}
	return corev1.ServiceTypeLoadBalancer // Default to LoadBalancer
}

// getAPIPort returns the control plane API port.
func getAPIPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.ControlPlane != nil && dittoServer.Spec.ControlPlane.Port > 0 {
		return dittoServer.Spec.ControlPlane.Port
	}
	return defaultAPIPort
}

// probeHandler builds the kubelet probe handler for a health path.
//
//   - Plain / native-TLS (no mTLS): an HTTPGet probe. The kubelet skips
//     certificate verification for HTTPS probes, so a private server CA needs
//     no extra trust here; only the scheme must match the listener.
//   - Mutual TLS (client_ca set): the listener requires a verified client
//     certificate, which the kubelet cannot present — an HTTPS HTTPGet probe
//     would fail the handshake and the pod would never become ready. Fall back
//     to a TCPSocket probe (port-accepting liveness) so mTLS does not wedge the
//     rollout. The deeper /health semantics are traded for schedulability.
func probeHandler(dittoServer *dittoiov1alpha1.DittoServer, path string) corev1.ProbeHandler {
	port := intstr.FromInt32(getAPIPort(dittoServer))
	if dittoServer.MutualTLSEnabled() {
		return corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: port}}
	}
	scheme := corev1.URISchemeHTTP
	if dittoServer.NativeTLSEnabled() {
		scheme = corev1.URISchemeHTTPS
	}
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: path, Port: port, Scheme: scheme},
	}
}

// PreStop hook delay (seconds) applied to the dfs container before SIGTERM.
const preStopSleepSeconds = 5

// Shutdown-budget multiplier: the dfs server runs shutdown stages serially,
// each bounded by shutdown_timeout. Cover the worst-case multi-stage path (~3x).
const shutdownStageMultiplier = 3

// Extra headroom (seconds) above the computed shutdown budget.
const terminationGraceBufferSeconds = 10

// getTerminationGracePeriodSeconds returns the pod TerminationGracePeriodSeconds for
// the dfs server. When the user sets it explicitly on the spec, that value is honored.
// Otherwise it is derived from the configured shutdown_timeout so the grace period and
// the server's per-stage shutdown budget stay coupled:
//
//	TGPS = preStop + shutdownStageMultiplier*shutdownTimeout + buffer
//
// With the default 30s shutdown_timeout this is 5 + 3*30 + 10 = 105s, which
// comfortably exceeds preStop + shutdown_timeout and avoids a SIGKILL mid-flush
// (metadata loss) on rollout, drain, or scale-down.
func getTerminationGracePeriodSeconds(dittoServer *dittoiov1alpha1.DittoServer) int64 {
	if dittoServer.Spec.TerminationGracePeriodSeconds != nil {
		return *dittoServer.Spec.TerminationGracePeriodSeconds
	}
	return preStopSleepSeconds +
		shutdownStageMultiplier*config.DefaultShutdownTimeoutSeconds +
		terminationGraceBufferSeconds
}

// createOrUpdateService is a helper that creates or updates a Service with retry logic.
// It handles owner reference setting and merges service specs to preserve cloud controller fields.
func (r *DittoServerReconciler) createOrUpdateService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer, svc *corev1.Service) error {
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
		},
	}

	return retryOnConflict(func() error {
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
			if err := controllerutil.SetControllerReference(dittoServer, existing, r.Scheme); err != nil {
				return err
			}
			// Only update fields we own, preserve cloud controller fields
			mergeServiceSpec(&existing.Spec, &svc.Spec)
			existing.Labels = svc.Labels
			existing.Annotations = mergeAnnotations(existing.Annotations, svc.Annotations)
			return nil
		})
		return err
	})
}

// reconcileHeadlessService creates/updates the headless Service for StatefulSet DNS.
func (r *DittoServerReconciler) reconcileHeadlessService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := podSelectorLabels(dittoServer.Name)

	svc := resources.NewServiceBuilder(dittoServer.Name+"-headless", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		AsHeadless().
		AddTCPPort("api", getAPIPort(dittoServer)).
		Build()

	return r.createOrUpdateService(ctx, dittoServer, svc)
}

// reconcileAPIService creates/updates the Service for REST API access.
func (r *DittoServerReconciler) reconcileAPIService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := podSelectorLabels(dittoServer.Name)
	apiPort := getAPIPort(dittoServer)

	svc := resources.NewServiceBuilder(dittoServer.Name+"-api", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		WithType(getServiceType(dittoServer)).
		WithAnnotations(dittoServer.Spec.Service.Annotations).
		AddTCPPort("api", apiPort).
		Build()

	return r.createOrUpdateService(ctx, dittoServer, svc)
}

func (r *DittoServerReconciler) reconcileStatefulSet(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer, replicas int32) (string, error) {
	logger := logf.FromContext(ctx)

	// Generate config to compute hash (same config that was just written to ConfigMap)
	configYAML, err := config.GenerateDittoFSConfig(dittoServer)
	if err != nil {
		return "", fmt.Errorf("failed to generate config for hash: %w", err)
	}

	// Collect secret data for hash computation
	secretData, err := r.collectSecretData(ctx, dittoServer)
	if err != nil {
		logger.Error(err, "Failed to collect secret data for hash, using config-only hash")
		secretData = make(map[string][]byte)
	}

	// Compute config hash BEFORE CreateOrUpdate
	configHash := resources.ComputeConfigHash(configYAML, secretData, dittoServer.Generation)

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dittoServer.Name,
			Namespace: dittoServer.Namespace,
		},
	}

	err = retryOnConflict(func() error {
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, statefulSet, func() error {
			if err := controllerutil.SetControllerReference(dittoServer, statefulSet, r.Scheme); err != nil {
				return err
			}

			labels := podSelectorLabels(dittoServer.Name)

			volumeMounts := []corev1.VolumeMount{
				{
					Name:      "controlplane",
					MountPath: "/data/controlplane",
				},
				{
					Name:      "metadata",
					MountPath: "/data/store/metadata",
				},
				{
					Name:      "config",
					MountPath: "/config",
				},
			}

			if dittoServer.Spec.Storage.ContentSize != "" {
				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      "content",
					MountPath: "/data/store/block",
				})
			}

			// Native TLS: mount the server cert/key Secret (and optional
			// client-CA Secret) read-only so the rendered controlplane.tls.*
			// paths resolve and the pod serves HTTPS end-to-end.
			if dittoServer.NativeTLSEnabled() {
				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      "tls-cert",
					MountPath: dittoiov1alpha1.TLSCertMountPath,
					ReadOnly:  true,
				})
				if dittoServer.MutualTLSEnabled() {
					volumeMounts = append(volumeMounts, corev1.VolumeMount{
						Name:      "tls-client-ca",
						MountPath: dittoiov1alpha1.TLSClientCAMountPath,
						ReadOnly:  true,
					})
				}
			}

			// Metrics bearer token: mount the referenced Secret read-only so the
			// rendered metrics.token_file path resolves to the scrape token. Only
			// when metrics are enabled — otherwise the listener is off and mounting
			// the Secret would expose it into the pod for no reason.
			if dittoServer.MetricsEnabled() && dittoServer.MetricsBearerTokenSecret() != nil {
				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      metricsTokenVolumeName,
					MountPath: dittoiov1alpha1.MetricsTokenMountPath,
					ReadOnly:  true,
				})
			}

			metadataSize, err := resource.ParseQuantity(dittoServer.Spec.Storage.MetadataSize)
			if err != nil {
				return fmt.Errorf("invalid metadata size: %w", err)
			}

			volumeClaimTemplates := []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "metadata",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						StorageClassName: dittoServer.Spec.Storage.StorageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: metadataSize,
							},
						},
					},
				},
			}

			if dittoServer.Spec.Storage.ContentSize != "" {
				contentSize, err := resource.ParseQuantity(dittoServer.Spec.Storage.ContentSize)
				if err != nil {
					return fmt.Errorf("invalid content size: %w", err)
				}

				volumeClaimTemplates = append(volumeClaimTemplates, corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "content",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						StorageClassName: dittoServer.Spec.Storage.StorageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: contentSize,
							},
						},
					},
				})
			}

			// Control-plane PVC (ALWAYS required): holds the control-plane SQLite DB
			// — the metadata-store registry + share definitions. Mounted at
			// /data/controlplane; small by nature (default 1Gi). Without a
			// persistent volume here, every pod restart wipes all stores and shares.
			controlPlaneSize := dittoServer.Spec.Storage.ControlPlaneSize
			if controlPlaneSize == "" {
				controlPlaneSize = "1Gi"
			}
			cpSize, err := resource.ParseQuantity(controlPlaneSize)
			if err != nil {
				return fmt.Errorf("invalid control plane size: %w", err)
			}

			volumeClaimTemplates = append(volumeClaimTemplates, corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "controlplane",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					StorageClassName: dittoServer.Spec.Storage.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: cpSize,
						},
					},
				},
			})

			// Build init containers
			var initContainers []corev1.Container
			if isPerconaEnabled(dittoServer) {
				initContainers = append(initContainers, buildPostgresInitContainer(dittoServer.Name))
			}

			// Merge env vars: secrets first, then Percona
			envVars := buildSecretEnvVars(dittoServer)
			if isPerconaEnabled(dittoServer) {
				envVars = append(envVars, buildPostgresEnvVars(dittoServer.Name)...)
			}

			// Pod volumes: the config ConfigMap, plus the native-TLS cert
			// (and optional client-CA) Secret(s) when TLS is enabled.
			podVolumes := []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: dittoServer.Name + "-config",
							},
						},
					},
				},
			}
			if dittoServer.NativeTLSEnabled() {
				podVolumes = append(podVolumes, corev1.Volume{
					Name: "tls-cert",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: dittoServer.Spec.ControlPlane.CertSecretName,
						},
					},
				})
				if dittoServer.MutualTLSEnabled() {
					podVolumes = append(podVolumes, corev1.Volume{
						Name: "tls-client-ca",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: dittoServer.Spec.ControlPlane.ClientCASecretName,
							},
						},
					})
				}
			}
			// Metrics bearer token: project the referenced Secret key to a single
			// file the rendered metrics.token_file points at. Gated on MetricsEnabled
			// so a disabled listener never projects the Secret into the pod.
			if ref := dittoServer.MetricsBearerTokenSecret(); dittoServer.MetricsEnabled() && ref != nil {
				podVolumes = append(podVolumes, corev1.Volume{
					Name: metricsTokenVolumeName,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: ref.Name,
							Items: []corev1.KeyToPath{
								{Key: ref.Key, Path: dittoiov1alpha1.MetricsTokenFileName},
							},
						},
					},
				})
			}

			statefulSet.Spec = appsv1.StatefulSetSpec{
				Replicas:    &replicas,
				ServiceName: dittoServer.Name + "-headless",
				Selector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
						Annotations: map[string]string{
							resources.ConfigHashAnnotation: configHash,
						},
					},
					Spec: corev1.PodSpec{
						// dfs imports no client-go and never calls the K8s API,
						// so it has no legitimate consumer for a ServiceAccount
						// token. Disabling automount keeps the unused namespace
						// default SA token off the network-exposed container.
						AutomountServiceAccountToken:  ptr.To(false),
						SecurityContext:               getPodSecurityContext(dittoServer),
						TerminationGracePeriodSeconds: ptr.To(getTerminationGracePeriodSeconds(dittoServer)),
						InitContainers:                initContainers,
						Containers: []corev1.Container{
							{
								Name:            "dittofs",
								Image:           dittoServer.Spec.Image,
								Command:         []string{"/app/dfs"},
								Args:            []string{"start", "--foreground", "--config", "/config/config.yaml"},
								VolumeMounts:    volumeMounts,
								Resources:       dittoServer.Spec.Resources,
								SecurityContext: getContainerSecurityContext(dittoServer),
								Ports:           buildContainerPorts(dittoServer, existingAdapterPorts(statefulSet)),
								Env:             envVars,
								LivenessProbe: &corev1.Probe{
									ProbeHandler:        probeHandler(dittoServer, "/health"),
									InitialDelaySeconds: 15,
									PeriodSeconds:       10,
									TimeoutSeconds:      5,
									SuccessThreshold:    1,
									FailureThreshold:    3,
								},
								ReadinessProbe: &corev1.Probe{
									ProbeHandler:        probeHandler(dittoServer, "/health/ready"),
									InitialDelaySeconds: 10,
									PeriodSeconds:       5,
									TimeoutSeconds:      5,
									SuccessThreshold:    1,
									FailureThreshold:    3,
								},
								StartupProbe: &corev1.Probe{
									ProbeHandler:        probeHandler(dittoServer, "/health"),
									InitialDelaySeconds: 0,
									PeriodSeconds:       5,
									TimeoutSeconds:      5,
									SuccessThreshold:    1,
									FailureThreshold:    30, // 30 * 5s = 150s max startup time
								},
								Lifecycle: &corev1.Lifecycle{
									PreStop: &corev1.LifecycleHandler{
										Exec: &corev1.ExecAction{
											Command: []string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", preStopSleepSeconds)},
										},
									},
								},
							},
						},
						Volumes: podVolumes,
					},
				},
				VolumeClaimTemplates: volumeClaimTemplates,
				PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
					WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
					WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				},
			}

			return nil
		})
		return err
	})

	return configHash, err
}

// getPodSecurityContext returns the pod security context with fsGroup set to ensure
// that mounted volumes are writable by the container user.
func getPodSecurityContext(dittoServer *dittoiov1alpha1.DittoServer) *corev1.PodSecurityContext {
	if dittoServer.Spec.PodSecurityContext != nil {
		return dittoServer.Spec.PodSecurityContext
	}

	fsGroup := int64(defaultFSGroup)
	return &corev1.PodSecurityContext{
		FSGroup: &fsGroup,
	}
}

// getContainerSecurityContext returns the SecurityContext for the managed dfs
// container. It applies a secure-by-default posture (drop ALL capabilities, no
// privilege escalation, run as non-root, RuntimeDefault seccomp) so the
// operator-generated StatefulSet is admitted by Pod-Security-Standards
// "restricted" namespaces and matches the hardened operator pod. The dfs image
// already ships USER 65532, so non-root is consistent.
//
// readOnlyRootFilesystem is intentionally NOT set: dfs writes to os.TempDir()
// (the block-store GC engine and config default-dir fallback) outside its
// mounted volumes, which a read-only root filesystem would break.
//
// When the user supplies Spec.SecurityContext, those fields override the
// secure defaults (user wins on conflicts).
func getContainerSecurityContext(dittoServer *dittoiov1alpha1.DittoServer) *corev1.SecurityContext {
	// Start from the user's context (if any) so all user-set fields — including
	// any added in future API versions — are preserved, then backfill the
	// secure defaults only where the user left a field unset (user wins).
	sc := dittoServer.Spec.SecurityContext.DeepCopy()
	if sc == nil {
		sc = &corev1.SecurityContext{}
	}

	if sc.AllowPrivilegeEscalation == nil {
		sc.AllowPrivilegeEscalation = ptr.To(false)
	}
	if sc.RunAsNonRoot == nil {
		sc.RunAsNonRoot = ptr.To(true)
	}
	if sc.Capabilities == nil {
		sc.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}}
	} else if len(sc.Capabilities.Drop) == 0 {
		// User set Capabilities (e.g. to Add one) but left Drop unset; backfill
		// the drop-ALL baseline so the pod still satisfies the restricted
		// Pod-Security-Standard while preserving the user's Add list.
		sc.Capabilities.Drop = []corev1.Capability{capabilityAll}
	}
	if sc.SeccompProfile == nil {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	// readOnlyRootFilesystem is intentionally left as the user set it (default
	// unset): dfs writes to os.TempDir() outside its mounted volumes, which a
	// read-only root filesystem would break.

	return sc
}

// existingAdapterPorts extracts existing container ports from the StatefulSet.
// Returns nil if the StatefulSet has no containers.
func existingAdapterPorts(sts *appsv1.StatefulSet) []corev1.ContainerPort {
	if len(sts.Spec.Template.Spec.Containers) == 0 {
		return nil
	}
	return sts.Spec.Template.Spec.Containers[0].Ports
}

// buildContainerPorts constructs the container ports for the DittoFS server.
// Emits infrastructure ports (api) and preserves any existing dynamic
// adapter ports (prefixed with "adapter-") from the current StatefulSet.
// Dynamic adapter ports are managed by reconcileContainerPorts in service_reconciler.go.
func buildContainerPorts(dittoServer *dittoiov1alpha1.DittoServer, existingPorts []corev1.ContainerPort) []corev1.ContainerPort {
	apiPort := getAPIPort(dittoServer)

	ports := []corev1.ContainerPort{
		{
			Name:          "api",
			ContainerPort: apiPort,
			Protocol:      corev1.ProtocolTCP,
		},
	}

	// Expose the metrics endpoint port when metrics are enabled, so the metrics
	// Service / ServiceMonitor target a named container port.
	if dittoServer.MetricsEnabled() {
		ports = append(ports, corev1.ContainerPort{
			Name:          metricsPortName,
			ContainerPort: dittoServer.MetricsPort(),
			Protocol:      corev1.ProtocolTCP,
		})
	}

	// Preserve existing dynamic adapter ports to avoid unnecessary StatefulSet restarts.
	for _, p := range existingPorts {
		if strings.HasPrefix(p.Name, adapterPortPrefix) {
			ports = append(ports, p)
		}
	}

	return ports
}

// secretEnvVar creates an environment variable sourced from a Kubernetes Secret.
func secretEnvVar(envName, secretName, key string, optional bool) corev1.EnvVar {
	env := corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretName,
				},
				Key: key,
			},
		},
	}
	if optional {
		env.ValueFrom.SecretKeyRef.Optional = &optional
	}
	return env
}

// buildSecretEnvVars creates environment variables for secrets that should NOT be in the ConfigMap.
// These are sourced from Kubernetes Secrets and injected as env vars into the container.
func buildSecretEnvVars(dittoServer *dittoiov1alpha1.DittoServer) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	// JWT secret (always present - either user-provided or auto-generated)
	jwtSecretRef := dittoServer.GetEffectiveJWTSecretRef()
	envVars = append(envVars, secretEnvVar(
		"DITTOFS_CONTROLPLANE_SECRET",
		jwtSecretRef.Name,
		jwtSecretRef.Key,
		false,
	))

	// Admin password (optional - only if user configured it). The referenced
	// Secret key must hold the admin password in CLEARTEXT: the server bootstraps
	// the admin user from DITTOFS_ADMIN_INITIAL_PASSWORD (hashing it itself) and
	// has no consumed password-hash bootstrap path. Injecting a bcrypt hash here
	// was a no-op — the server ignored it and generated a random password that
	// neither the user nor the operator knew, so admin login (and thus operator
	// service-account provisioning) failed and the server never reached Ready.
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.PasswordSecretRef != nil {
		ref := dittoServer.Spec.Identity.Admin.PasswordSecretRef
		envVars = append(envVars, secretEnvVar(
			"DITTOFS_ADMIN_INITIAL_PASSWORD",
			ref.Name,
			ref.Key,
			false,
		))
	} else {
		// Inject auto-generated admin password from operator-managed Secret.
		// Optional=true so the pod doesn't fail if the Secret is manually deleted.
		envVars = append(envVars, secretEnvVar(
			"DITTOFS_ADMIN_INITIAL_PASSWORD",
			dittoServer.GetAdminCredentialsSecretName(),
			"password",
			true,
		))
	}

	// LDAP bind password (optional - only if LDAP configured with a Secret ref).
	// Injected as DITTOFS_LDAP_BIND_PASSWORD so the cleartext bind password never
	// lands in the config ConfigMap.
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.LDAP != nil &&
		dittoServer.Spec.Identity.LDAP.BindPasswordSecretRef != nil {
		ref := dittoServer.Spec.Identity.LDAP.BindPasswordSecretRef
		envVars = append(envVars, secretEnvVar(
			"DITTOFS_LDAP_BIND_PASSWORD",
			ref.Name,
			ref.Key,
			false,
		))
	}

	// PostgreSQL connection fields (only if Postgres configured and NOT using Percona)
	// Percona case already injects DATABASE_URL via buildPostgresEnvVars.
	// Inject individual env vars that Viper maps to database.postgres.* struct fields.
	if dittoServer.Spec.Database != nil && dittoServer.Spec.Database.PostgresSecretRef != nil &&
		!isPerconaEnabled(dittoServer) {
		ref := dittoServer.Spec.Database.PostgresSecretRef
		envVars = append(envVars,
			secretEnvVar("DITTOFS_DATABASE_POSTGRES_HOST", ref.Name, "host", false),
			secretEnvVar("DITTOFS_DATABASE_POSTGRES_DATABASE", ref.Name, "database", false),
			secretEnvVar("DITTOFS_DATABASE_POSTGRES_USER", ref.Name, "user", false),
			secretEnvVar("DITTOFS_DATABASE_POSTGRES_PASSWORD", ref.Name, "password", false),
		)
		// Optional: port and sslmode (have defaults in DittoFS config)
		envVars = append(envVars,
			secretEnvVar("DITTOFS_DATABASE_POSTGRES_PORT", ref.Name, "port", true),
			secretEnvVar("DITTOFS_DATABASE_POSTGRES_SSLMODE", ref.Name, "sslmode", true),
		)
	}

	return envVars
}

// buildPostgresInitContainer creates an init container that waits for PostgreSQL to be ready.
// Uses pg_isready with full auth check (connects as dittofs user to dittofs database).
func buildPostgresInitContainer(dsName string) corev1.Container {
	secretName := percona.SecretName(dsName)

	return corev1.Container{
		Name:  "wait-for-postgres",
		Image: "postgres:16-alpine",
		Command: []string{
			"/bin/sh",
			"-c",
			`echo "Waiting for PostgreSQL at $PGHOST:$PGPORT..."
timeout=300
elapsed=0
while ! pg_isready -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -t 5; do
  echo "PostgreSQL not ready, waiting... ($elapsed/$timeout seconds)"
  sleep 2
  elapsed=$((elapsed + 2))
  if [ $elapsed -ge $timeout ]; then
    echo "Timeout waiting for PostgreSQL"
    exit 1
  fi
done
echo "PostgreSQL is ready!"`,
		},
		Env: []corev1.EnvVar{
			{
				Name: "PGHOST",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "host",
					},
				},
			},
			{
				Name: "PGPORT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "port",
					},
				},
			},
			{
				Name: "PGUSER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "user",
					},
				},
			},
			{
				Name: "PGPASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "password",
					},
				},
			},
			{
				Name: "PGDATABASE",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "dbname",
					},
				},
			},
		},
	}
}

// buildPostgresEnvVars creates the DATABASE_URL env var from Percona Secret.
// Uses the 'uri' key which contains the full connection string.
func buildPostgresEnvVars(dsName string) []corev1.EnvVar {
	secretName := percona.SecretName(dsName)

	return []corev1.EnvVar{
		secretEnvVar("DATABASE_URL", secretName, "uri", false),
	}
}

// reconcilePerconaPGCluster creates/updates the PerconaPGCluster CR if Percona is enabled.
// The PerconaPGCluster is owned by DittoServer and will be deleted when DittoServer is deleted.
// Per CONTEXT.md decision: operator doesn't reconcile user modifications (no drift reconciliation).
func (r *DittoServerReconciler) reconcilePerconaPGCluster(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	// Skip if Percona is not enabled
	if dittoServer.Spec.Percona == nil || !dittoServer.Spec.Percona.Enabled {
		return nil
	}

	logger := logf.FromContext(ctx)
	clusterName := percona.ClusterName(dittoServer.Name)

	pgCluster := &pgv2.PerconaPGCluster{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      clusterName,
	}, pgCluster)

	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get PerconaPGCluster: %w", err)
	}

	// Create if doesn't exist
	if apierrors.IsNotFound(err) {
		logger.Info("Creating PerconaPGCluster", "name", clusterName)

		pgClusterSpec, err := percona.BuildPerconaPGClusterSpec(dittoServer)
		if err != nil {
			return fmt.Errorf("failed to build PerconaPGCluster spec: %w", err)
		}

		pgCluster = &pgv2.PerconaPGCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: dittoServer.Namespace,
			},
			Spec: pgClusterSpec,
		}

		// Set owner reference so it's deleted when DittoServer is deleted
		if err := controllerutil.SetControllerReference(dittoServer, pgCluster, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference: %w", err)
		}

		if err := r.Create(ctx, pgCluster); err != nil {
			return fmt.Errorf("failed to create PerconaPGCluster: %w", err)
		}

		logger.Info("Created PerconaPGCluster", "name", clusterName)
		return nil
	}

	// PerconaPGCluster exists - do NOT update (no drift reconciliation per CONTEXT.md)
	// Users can modify it directly if needed
	logger.V(1).Info("PerconaPGCluster already exists, skipping update", "name", clusterName)
	return nil
}

// collectSecretData gathers all secret data referenced by the DittoServer CR.
// This is used to compute the config hash - when any secret changes, the hash changes.
func (r *DittoServerReconciler) collectSecretData(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) (map[string][]byte, error) {
	secrets := make(map[string][]byte)

	// JWT secret (use effective ref which handles both user-provided and auto-generated)
	jwtSecretRef := dittoServer.GetEffectiveJWTSecretRef()
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      jwtSecretRef.Name,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get JWT secret: %w", err)
	}
	if data, ok := secret.Data[jwtSecretRef.Key]; ok {
		secrets["jwt:"+jwtSecretRef.Key] = data
	}

	// Admin password secret
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.PasswordSecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.Identity.Admin.PasswordSecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get admin password secret: %w", err)
		}
		key := dittoServer.Spec.Identity.Admin.PasswordSecretRef.Key
		if data, ok := secret.Data[key]; ok {
			secrets["admin:"+key] = data
		}
	} else {
		// Include auto-generated admin credentials in hash
		adminSecret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.GetAdminCredentialsSecretName(),
		}, adminSecret); err == nil {
			if data, ok := adminSecret.Data["password"]; ok {
				secrets["admin:password"] = data
			}
		}
		// Note: Don't error if secret doesn't exist yet - it's created in reconcileAdminCredentials
	}

	// PostgreSQL credentials secret (if configured)
	if dittoServer.Spec.Database != nil && dittoServer.Spec.Database.PostgresSecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.Database.PostgresSecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get postgres secret: %w", err)
		}
		// Hash all keys so any credential change triggers pod restart.
		for k, v := range secret.Data {
			secrets["postgres:"+k] = v
		}
	}

	// Percona PostgreSQL credentials secret (if Percona enabled)
	if isPerconaEnabled(dittoServer) {
		secretName := percona.SecretName(dittoServer.Name)
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      secretName,
		}, secret); err == nil {
			// Include uri key for hash (credential changes should restart pod)
			if data, ok := secret.Data["uri"]; ok {
				secrets["percona:uri"] = data
			}
		}
		// Note: Don't error if secret doesn't exist yet - Percona operator creates it
	}

	return secrets, nil
}

// mergeServiceSpec updates only the fields we own in the Service spec,
// preserving fields managed by cloud controllers (ClusterIP, HealthCheckNodePort, etc.).
// This prevents optimistic locking conflicts when external controllers modify the service.
func mergeServiceSpec(existing, desired *corev1.ServiceSpec) {
	// Update fields we own
	existing.Type = desired.Type
	existing.Selector = desired.Selector
	existing.Ports = mergePorts(existing.Ports, desired.Ports)

	// Preserve ClusterIP - only set if not already assigned
	// ClusterIP is immutable after creation, setting it would fail anyway
	// For headless services (ClusterIP: None), we need to set it on create
	if existing.ClusterIP == "" {
		existing.ClusterIP = desired.ClusterIP
	}

	// Note: We intentionally DO NOT update these fields as they are managed by cloud controllers:
	// - ClusterIP, ClusterIPs (assigned by Kubernetes, immutable)
	// - HealthCheckNodePort (assigned by cloud LB controller)
	// - LoadBalancerIP (deprecated, but may be set externally)
	// - LoadBalancerClass (may be defaulted by admission webhook)
	// - ExternalTrafficPolicy, InternalTrafficPolicy (preserve if set)
	// - AllocateLoadBalancerNodePorts (preserve if set)
	// - IPFamilies, IPFamilyPolicy (assigned by Kubernetes)

	// Update ExternalTrafficPolicy only if we're explicitly setting it
	if desired.ExternalTrafficPolicy != "" {
		existing.ExternalTrafficPolicy = desired.ExternalTrafficPolicy
	}
}

// mergePorts merges desired ports into existing, preserving NodePort assignments.
// This ensures that cloud-assigned NodePorts are not changed, preventing conflicts.
func mergePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	if len(existing) == 0 {
		return desired
	}

	// Build map of existing ports by name for quick lookup
	existingByName := make(map[string]corev1.ServicePort)
	for _, p := range existing {
		existingByName[p.Name] = p
	}

	// Merge desired into existing, preserving NodePort
	result := make([]corev1.ServicePort, 0, len(desired))
	for _, d := range desired {
		merged := d
		if e, ok := existingByName[d.Name]; ok {
			// Preserve NodePort if it was assigned
			if e.NodePort != 0 && d.NodePort == 0 {
				merged.NodePort = e.NodePort
			}
		}
		result = append(result, merged)
	}

	return result
}

// mergeAnnotations merges desired annotations into existing, without removing
// annotations that may have been added by cloud controllers.
// Note: This modifies the existing map in place. If existing is nil, use the
// returned map. Returns the merged map (which may be existing or a new map).
func mergeAnnotations(existing, desired map[string]string) map[string]string {
	if len(desired) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(map[string]string, len(desired))
	}
	for k, v := range desired {
		existing[k] = v
	}
	return existing
}

// determinePhase returns the appropriate phase based on replica counts.
func determinePhase(desired, ready int32) string {
	if desired == 0 {
		return "Stopped"
	}
	if ready == desired {
		return "Running"
	}
	return "Pending"
}

// isPerconaEnabled returns true if Percona PostgreSQL integration is enabled.
func isPerconaEnabled(dittoServer *dittoiov1alpha1.DittoServer) bool {
	return dittoServer.Spec.Percona != nil && dittoServer.Spec.Percona.Enabled
}

// collectNotReadyReasons gathers the list of conditions that are not met.
func collectNotReadyReasons(configReady, available, progressing, databaseReady, authenticated bool) []string {
	var reasons []string
	if !configReady {
		reasons = append(reasons, "ConfigNotReady")
	}
	if !available {
		reasons = append(reasons, "NotAvailable")
	}
	if progressing {
		reasons = append(reasons, "StillProgressing")
	}
	if !databaseReady {
		reasons = append(reasons, "DatabaseNotReady")
	}
	if !authenticated {
		reasons = append(reasons, "NotAuthenticated")
	}
	return reasons
}
