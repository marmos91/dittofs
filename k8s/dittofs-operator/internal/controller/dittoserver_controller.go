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
	"fmt"
	"time"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/internal/controller/config"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/percona"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/resources"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/conditions"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/nfs"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/smb"
	pgv2 "github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
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
	// defaultMetricsPort is the default Prometheus metrics port
	defaultMetricsPort = 9090
	// defaultFSGroup is the default fsGroup for pod security context (nonroot user)
	defaultFSGroup = 65532
)

// retryOnConflict wraps an operation with retry logic for optimistic locking conflicts.
// This is necessary because CreateOrUpdate can race with status updates from other controllers.
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
		time.Sleep(time.Duration(i+1) * retryBackoffBase)
	}
	return err
}

// DittoServerReconciler reconciles a DittoServer object
type DittoServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
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
// +kubebuilder:rbac:groups=pgv2.percona.com,resources=perconapgclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgv2.percona.com,resources=perconapgclusters/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DittoServer object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
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
			return ctrl.Result{}, err
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

	if err := r.reconcileFileService(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile file Service: %v", err)
		logger.Error(err, "Failed to reconcile file Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileAPIService(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile API Service: %v", err)
		logger.Error(err, "Failed to reconcile API Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileMetricsService(ctx, dittoServer); err != nil {
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile metrics Service: %v", err)
		logger.Error(err, "Failed to reconcile metrics Service")
		return ctrl.Result{}, err
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
		r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "ReconcileFailed",
			"Failed to reconcile StatefulSet: %v", err)
		logger.Error(err, "Failed to reconcile StatefulSet")
		return ctrl.Result{}, err
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

	return ctrl.Result{}, nil
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

	dittoServerCopy.Status.NFSEndpoint = fmt.Sprintf("%s-file.%s.svc.cluster.local:%d",
		dittoServer.Name, dittoServer.Namespace, nfs.GetNFSPort(dittoServer))

	// Set ConfigReady condition
	r.updateConfigReadyCondition(ctx, dittoServer, &dittoServerCopy.Status)

	// Set DatabaseReady condition (only relevant when Percona enabled)
	r.updateDatabaseReadyCondition(ctx, dittoServer, &dittoServerCopy.Status)

	// Set Available condition
	updateAvailableCondition(dittoServer, &dittoServerCopy.Status, replicas, statefulSet)

	// Set Progressing condition
	updateProgressingCondition(dittoServer, &dittoServerCopy.Status, replicas, statefulSet)

	// Set Ready condition (aggregate of other conditions)
	updateReadyCondition(dittoServer, &dittoServerCopy.Status)

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
) {
	configReady := conditions.IsConditionTrue(status.Conditions, conditions.ConditionConfigReady)
	available := conditions.IsConditionTrue(status.Conditions, conditions.ConditionAvailable)
	progressing := conditions.IsConditionTrue(status.Conditions, conditions.ConditionProgressing)

	databaseReady := true
	if isPerconaEnabled(dittoServer) {
		databaseReady = conditions.IsConditionTrue(status.Conditions, conditions.ConditionDatabaseReady)
	}

	allReady := configReady && available && !progressing && databaseReady
	if allReady {
		conditions.SetCondition(&status.Conditions, dittoServer.Generation,
			conditions.ConditionReady, metav1.ConditionTrue, "AllConditionsMet",
			"DittoServer is fully operational")
	} else {
		reasons := collectNotReadyReasons(configReady, available, progressing, databaseReady)
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

	// Update phase to Deleting
	dittoServerCopy := dittoServer.DeepCopy()
	dittoServerCopy.Status.Phase = "Deleting"
	if err := r.Status().Update(ctx, dittoServerCopy); err != nil {
		logger.Error(err, "Failed to update phase to Deleting")
		// Continue with cleanup even if status update fails
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
		controllerutil.RemoveFinalizer(dittoServer, finalizerName)
		if err := r.Update(ctx, dittoServer); err != nil {
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
	controllerutil.RemoveFinalizer(dittoServer, finalizerName)
	if err := r.Update(ctx, dittoServer); err != nil {
		return false, err
	}

	return false, nil
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
		Named("dittoserver")

	// Conditionally watch PerconaPGCluster only if CRD exists
	// This allows the operator to work without Percona Operator installed
	_, err := mgr.GetRESTMapper().RESTMapping(pgv2.GroupVersion.WithKind("PerconaPGCluster").GroupKind())
	if err == nil {
		builder = builder.Owns(&pgv2.PerconaPGCluster{})
	}

	return builder.Complete(r)
}

// reconcileJWTSecret ensures a JWT signing secret exists for the DittoServer.
// If the user hasn't provided a JWT secretRef, we auto-generate one and store it
// in a managed Kubernetes Secret.
func (r *DittoServerReconciler) reconcileJWTSecret(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	// If user has explicitly provided a JWT secret reference, use that
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.JWT != nil &&
		dittoServer.Spec.Identity.JWT.SecretRef.Name != "" {
		return nil
	}

	secretName := dittoServer.Name + "-jwt-secret"
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

// generateRandomSecret generates a cryptographically secure random string of the given length.
// Returns an error if random generation fails (should never happen with crypto/rand).
func generateRandomSecret(length int) (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	randomBytes := make([]byte, length)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	for i := range result {
		result[i] = chars[int(randomBytes[i])%len(chars)]
	}
	return string(result), nil
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

// getMetricsPort returns the metrics port.
func getMetricsPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.Metrics != nil && dittoServer.Spec.Metrics.Port > 0 {
		return dittoServer.Spec.Metrics.Port
	}
	return defaultMetricsPort
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
	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	nfsPort := nfs.GetNFSPort(dittoServer)

	svc := resources.NewServiceBuilder(dittoServer.Name+"-headless", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		AsHeadless().
		AddTCPPort("nfs", nfsPort).
		Build()

	return r.createOrUpdateService(ctx, dittoServer, svc)
}

// reconcileFileService creates/updates the Service for file protocols (NFS, SMB).
func (r *DittoServerReconciler) reconcileFileService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	nfsPort := nfs.GetNFSPort(dittoServer)

	builder := resources.NewServiceBuilder(dittoServer.Name+"-file", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		WithType(getServiceType(dittoServer)).
		WithAnnotations(dittoServer.Spec.Service.Annotations).
		AddTCPPort("nfs", nfsPort)

	// Add SMB port if enabled
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled {
		smbPort := smb.GetSMBPort(dittoServer)
		builder.AddTCPPort("smb", smbPort)
	}

	svc := builder.Build()

	return r.createOrUpdateService(ctx, dittoServer, svc)
}

// reconcileAPIService creates/updates the Service for REST API access.
func (r *DittoServerReconciler) reconcileAPIService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

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

// reconcileMetricsService creates/updates the Service for Prometheus metrics (if enabled).
func (r *DittoServerReconciler) reconcileMetricsService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	// Only create metrics service if metrics are enabled
	if dittoServer.Spec.Metrics == nil || !dittoServer.Spec.Metrics.Enabled {
		// Delete metrics service if it exists
		existing := &corev1.Service{}
		err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Name + "-metrics",
		}, existing)
		if err == nil {
			// Service exists, delete it
			return r.Delete(ctx, existing)
		}
		return client.IgnoreNotFound(err)
	}

	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	metricsPort := getMetricsPort(dittoServer)

	// Metrics service is always ClusterIP (internal only)
	svc := resources.NewServiceBuilder(dittoServer.Name+"-metrics", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		WithType(corev1.ServiceTypeClusterIP).
		AddTCPPort("metrics", metricsPort).
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

			labels := map[string]string{
				"app":      "dittofs-server",
				"instance": dittoServer.Name,
			}

			volumeMounts := []corev1.VolumeMount{
				{
					Name:      "metadata",
					MountPath: "/data/metadata",
				},
				{
					Name:      "cache",
					MountPath: "/data/cache",
				},
				{
					Name:      "config",
					MountPath: "/config",
				},
			}

			if dittoServer.Spec.Storage.ContentSize != "" {
				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      "content",
					MountPath: "/data/content",
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

			// Cache PVC (ALWAYS required for WAL persistence)
			cacheSize, err := resource.ParseQuantity(dittoServer.Spec.Storage.CacheSize)
			if err != nil {
				return fmt.Errorf("invalid cache size: %w", err)
			}

			// Cache VolumeClaimTemplate - always required for WAL persistence
			volumeClaimTemplates = append(volumeClaimTemplates, corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cache",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					StorageClassName: dittoServer.Spec.Storage.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: cacheSize,
						},
					},
				},
			})

			// Build init containers
			var initContainers []corev1.Container
			if isPerconaEnabled(dittoServer) {
				initContainers = append(initContainers, buildPostgresInitContainer(dittoServer.Name))
			}

			// Merge env vars: secrets first, then S3, then Percona
			envVars := buildSecretEnvVars(dittoServer)
			envVars = append(envVars, buildS3EnvVars(dittoServer.Spec.S3)...)
			if isPerconaEnabled(dittoServer) {
				envVars = append(envVars, buildPostgresEnvVars(dittoServer.Name)...)
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
						SecurityContext: getPodSecurityContext(dittoServer),
						InitContainers:  initContainers,
						Containers: []corev1.Container{
							{
								Name:            "dittofs",
								Image:           dittoServer.Spec.Image,
								Command:         []string{"/app/dittofs"},
								Args:            []string{"start", "--foreground", "--config", "/config/config.yaml"},
								VolumeMounts:    volumeMounts,
								Resources:       dittoServer.Spec.Resources,
								SecurityContext: dittoServer.Spec.SecurityContext,
								Ports:           buildContainerPorts(dittoServer),
								Env:             envVars,
								LivenessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path: "/health",
											Port: intstr.FromInt32(getAPIPort(dittoServer)),
										},
									},
									InitialDelaySeconds: 15,
									PeriodSeconds:       10,
									TimeoutSeconds:      5,
									SuccessThreshold:    1,
									FailureThreshold:    3,
								},
								ReadinessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path: "/health/ready",
											Port: intstr.FromInt32(getAPIPort(dittoServer)),
										},
									},
									InitialDelaySeconds: 10,
									PeriodSeconds:       5,
									TimeoutSeconds:      5,
									SuccessThreshold:    1,
									FailureThreshold:    3,
								},
								StartupProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path: "/health",
											Port: intstr.FromInt32(getAPIPort(dittoServer)),
										},
									},
									InitialDelaySeconds: 0,
									PeriodSeconds:       5,
									TimeoutSeconds:      5,
									SuccessThreshold:    1,
									FailureThreshold:    30, // 30 * 5s = 150s max startup time
								},
								Lifecycle: &corev1.Lifecycle{
									PreStop: &corev1.LifecycleHandler{
										Exec: &corev1.ExecAction{
											Command: []string{"/bin/sh", "-c", "sleep 5"},
										},
									},
								},
							},
						},
						Volumes: []corev1.Volume{
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
						},
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

// buildContainerPorts constructs the container ports based on enabled protocols
func buildContainerPorts(dittoServer *dittoiov1alpha1.DittoServer) []corev1.ContainerPort {
	nfsPort := nfs.GetNFSPort(dittoServer)

	ports := []corev1.ContainerPort{
		{
			Name:          "nfs",
			ContainerPort: nfsPort,
			Protocol:      corev1.ProtocolTCP,
		},
	}

	// Add API port
	apiPort := getAPIPort(dittoServer)
	ports = append(ports, corev1.ContainerPort{
		Name:          "api",
		ContainerPort: apiPort,
		Protocol:      corev1.ProtocolTCP,
	})

	// Add metrics port if enabled
	if dittoServer.Spec.Metrics != nil && dittoServer.Spec.Metrics.Enabled {
		metricsPort := getMetricsPort(dittoServer)
		ports = append(ports, corev1.ContainerPort{
			Name:          "metrics",
			ContainerPort: metricsPort,
			Protocol:      corev1.ProtocolTCP,
		})
	}

	// Add SMB port if enabled
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled {
		smbPort := smb.GetSMBPort(dittoServer)
		ports = append(ports, corev1.ContainerPort{
			Name:          "smb",
			ContainerPort: smbPort,
			Protocol:      corev1.ProtocolTCP,
		})
	}

	return ports
}

// buildS3EnvVars creates environment variables for S3 credentials from Secret reference.
// Returns nil if S3 is not configured.
func buildS3EnvVars(spec *dittoiov1alpha1.S3StoreConfig) []corev1.EnvVar {
	if spec == nil || spec.CredentialsSecretRef == nil {
		return nil
	}

	ref := spec.CredentialsSecretRef

	// Apply defaults for key names
	accessKeyIDKey := defaultIfEmpty(ref.AccessKeyIDKey, "accessKeyId")
	secretAccessKeyKey := defaultIfEmpty(ref.SecretAccessKeyKey, "secretAccessKey")
	endpointKey := defaultIfEmpty(ref.EndpointKey, "endpoint")

	envVars := []corev1.EnvVar{
		secretEnvVar("AWS_ACCESS_KEY_ID", ref.SecretName, accessKeyIDKey, false),
		secretEnvVar("AWS_SECRET_ACCESS_KEY", ref.SecretName, secretAccessKeyKey, false),
		secretEnvVar("AWS_ENDPOINT_URL", ref.SecretName, endpointKey, true),
	}

	// Add region if specified
	if spec.Region != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AWS_REGION",
			Value: spec.Region,
		})
	}

	return envVars
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

// defaultIfEmpty returns the value if non-empty, otherwise returns the default.
func defaultIfEmpty(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

// buildSecretEnvVars creates environment variables for secrets that should NOT be in the ConfigMap.
// These are sourced from Kubernetes Secrets and injected as env vars into the container.
func buildSecretEnvVars(dittoServer *dittoiov1alpha1.DittoServer) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	// JWT secret (always present - either user-provided or auto-generated)
	jwtSecretRef := dittoServer.GetEffectiveJWTSecretRef()
	envVars = append(envVars, secretEnvVar(
		"DITTOFS_CONTROLPLANE_JWT_SECRET",
		jwtSecretRef.Name,
		jwtSecretRef.Key,
		false,
	))

	// Admin password hash (optional - only if user configured it)
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.PasswordSecretRef != nil {
		ref := dittoServer.Spec.Identity.Admin.PasswordSecretRef
		envVars = append(envVars, secretEnvVar(
			"DITTOFS_ADMIN_PASSWORD_HASH",
			ref.Name,
			ref.Key,
			false,
		))
	}

	// PostgreSQL connection string (only if Postgres configured and NOT using Percona)
	// Percona case already injects DATABASE_URL via buildPostgresEnvVars.
	if dittoServer.Spec.Database != nil && dittoServer.Spec.Database.PostgresSecretRef != nil &&
		!isPerconaEnabled(dittoServer) {
		ref := dittoServer.Spec.Database.PostgresSecretRef
		envVars = append(envVars, secretEnvVar(
			"DITTOFS_DATABASE_POSTGRES",
			ref.Name,
			ref.Key,
			false,
		))
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
	}

	// PostgreSQL connection string secret (if configured)
	if dittoServer.Spec.Database != nil && dittoServer.Spec.Database.PostgresSecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.Database.PostgresSecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get postgres secret: %w", err)
		}
		key := dittoServer.Spec.Database.PostgresSecretRef.Key
		if data, ok := secret.Data[key]; ok {
			secrets["postgres:"+key] = data
		}
	}

	// S3 credentials secret (if configured)
	if dittoServer.Spec.S3 != nil && dittoServer.Spec.S3.CredentialsSecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.S3.CredentialsSecretRef.SecretName,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get S3 credentials secret: %w", err)
		}

		// Include all data from the secret for hash
		for k, v := range secret.Data {
			secrets["s3:"+k] = v
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
func collectNotReadyReasons(configReady, available, progressing, databaseReady bool) []string {
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
	return reasons
}
