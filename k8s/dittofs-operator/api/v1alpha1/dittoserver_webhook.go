package v1alpha1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var dittoserverlog = logf.Log.WithName("dittoserver-resource")

// Default port values for validation
const (
	defaultNFSPort     = 12049 // DittoFS uses non-privileged port by default
	defaultAPIPort     = 8080
	defaultSMBPort     = 12445 // DittoFS uses non-privileged port by default
	defaultMetricsPort = 9090
	privilegedPortMax  = 1024
	minPort            = 1
	maxPort            = 65535
)

// DittoServerValidator implements webhook.CustomValidator with cluster client access.
// This enables validation of cluster resources like StorageClass and Secrets.
// +kubebuilder:object:generate=false
type DittoServerValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &DittoServerValidator{}

// SetupWebhookWithManager will setup the manager to manage the webhooks
// Deprecated: Use SetupDittoServerWebhookWithManager for cluster resource validation
func (r *DittoServer) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// SetupDittoServerWebhookWithManager sets up the webhook with client injection for validation.
// This function should be used instead of SetupWebhookWithManager when cluster resource
// validation (StorageClass, Secrets) is needed.
func SetupDittoServerWebhookWithManager(mgr ctrl.Manager) error {
	validator := &DittoServerValidator{
		Client: mgr.GetClient(),
	}
	return ctrl.NewWebhookManagedBy(mgr).
		For(&DittoServer{}).
		WithValidator(validator).
		Complete()
}

// +kubebuilder:webhook:path=/validate-dittofs-dittofs-com-v1alpha1-dittoserver,mutating=false,failurePolicy=fail,sideEffects=None,groups=dittofs.dittofs.com,resources=dittoservers,verbs=create;update,versions=v1alpha1,name=vdittoserver.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &DittoServer{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type
func (r *DittoServer) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	dittoserverlog.Info("validate create", "name", r.Name)
	return r.validateDittoServer()
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type
func (r *DittoServer) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	dittoserverlog.Info("validate update", "name", r.Name)
	return r.validateDittoServer()
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type
func (r *DittoServer) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	dittoserverlog.Info("validate delete", "name", r.Name)
	// No validation needed for delete
	return nil, nil
}

// validateDittoServer validates the DittoServer spec
// With infrastructure-only config, validation is minimal as dynamic config
// (stores, shares, adapters, users) is managed via REST API at runtime.
func (r *DittoServer) validateDittoServer() (admission.Warnings, error) {
	var warnings admission.Warnings

	// Validate storage is provided
	if r.Spec.Storage.MetadataSize == "" {
		return warnings, fmt.Errorf("storage.metadataSize is required")
	}

	// Validate database configuration consistency
	if r.Spec.Database != nil {
		// If PostgresSecretRef is set, warn that Type field will be ignored
		if r.Spec.Database.PostgresSecretRef != nil && r.Spec.Database.Type == "sqlite" {
			warnings = append(warnings, "database.postgresSecretRef is set but database.type is 'sqlite' - PostgreSQL will take precedence")
		}
	}

	// Validate control plane port range
	if r.Spec.ControlPlane != nil && r.Spec.ControlPlane.Port != 0 {
		if r.Spec.ControlPlane.Port < minPort || r.Spec.ControlPlane.Port > maxPort {
			return warnings, fmt.Errorf("controlPlane.port must be between %d and %d", minPort, maxPort)
		}
	}

	// Validate metrics port range
	if r.Spec.Metrics != nil && r.Spec.Metrics.Port != 0 {
		if r.Spec.Metrics.Port < minPort || r.Spec.Metrics.Port > maxPort {
			return warnings, fmt.Errorf("metrics.port must be between %d and %d", minPort, maxPort)
		}
	}

	// Validate port uniqueness and warn about privileged ports
	portWarnings, err := r.validatePorts()
	if err != nil {
		return warnings, err
	}
	warnings = append(warnings, portWarnings...)

	return warnings, nil
}

// validatePorts checks port configuration for conflicts and warns about privileged ports.
func (r *DittoServer) validatePorts() (admission.Warnings, error) {
	var warnings admission.Warnings

	// Build port map with enabled ports
	ports := map[int32]string{
		portOrDefault(r.Spec.NFSPort, defaultNFSPort):                        "nfs",
		portOrDefault(controlPlanePort(r.Spec.ControlPlane), defaultAPIPort): "api",
	}

	// Check SMB port uniqueness
	if r.Spec.SMB != nil && r.Spec.SMB.Enabled {
		smbPort := portOrDefault(r.Spec.SMB.Port, defaultSMBPort)
		if existing, ok := ports[smbPort]; ok {
			return nil, fmt.Errorf("port %d is used by both %s and smb", smbPort, existing)
		}
		ports[smbPort] = "smb"
	}

	// Check metrics port uniqueness
	if r.Spec.Metrics != nil && r.Spec.Metrics.Enabled {
		metricsPort := portOrDefault(metricsPortPtr(r.Spec.Metrics), defaultMetricsPort)
		if existing, ok := ports[metricsPort]; ok {
			return nil, fmt.Errorf("port %d is used by both %s and metrics", metricsPort, existing)
		}
		ports[metricsPort] = "metrics"
	}

	// Warn about privileged ports
	for port, name := range ports {
		if port < privilegedPortMax {
			warnings = append(warnings,
				fmt.Sprintf("%s port %d is privileged; may require CAP_NET_BIND_SERVICE capability", name, port))
		}
	}

	return warnings, nil
}

// portOrDefault returns the value of a port pointer or the default value.
func portOrDefault(port *int32, defaultValue int32) int32 {
	if port != nil {
		return *port
	}
	return defaultValue
}

// controlPlanePort returns a pointer to the control plane port, or nil if not set.
func controlPlanePort(cp *ControlPlaneAPIConfig) *int32 {
	if cp != nil && cp.Port > 0 {
		return &cp.Port
	}
	return nil
}

// metricsPortPtr returns a pointer to the metrics port, or nil if not set.
func metricsPortPtr(m *MetricsConfig) *int32 {
	if m != nil && m.Port > 0 {
		return &m.Port
	}
	return nil
}

// ValidateCreate implements webhook.CustomValidator for DittoServerValidator
func (v *DittoServerValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	ds := obj.(*DittoServer)
	dittoserverlog.Info("validate create (with client)", "name", ds.Name)
	return v.validateDittoServerWithClient(ctx, ds)
}

// ValidateUpdate implements webhook.CustomValidator for DittoServerValidator
func (v *DittoServerValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	ds := newObj.(*DittoServer)
	dittoserverlog.Info("validate update (with client)", "name", ds.Name)
	return v.validateDittoServerWithClient(ctx, ds)
}

// ValidateDelete implements webhook.CustomValidator for DittoServerValidator
func (v *DittoServerValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// No validation needed for delete
	return nil, nil
}

// validateDittoServerWithClient performs validation that requires cluster access.
// This includes StorageClass validation and S3 Secret validation.
func (v *DittoServerValidator) validateDittoServerWithClient(ctx context.Context, ds *DittoServer) (admission.Warnings, error) {
	var warnings admission.Warnings

	// First, run all the standard validations (storage required, port conflicts, etc.)
	stdWarnings, err := ds.validateDittoServer()
	if err != nil {
		return stdWarnings, err
	}
	warnings = append(warnings, stdWarnings...)

	// Validate StorageClass if explicitly specified
	if ds.Spec.Storage.StorageClassName != nil && *ds.Spec.Storage.StorageClassName != "" {
		scName := *ds.Spec.Storage.StorageClassName
		storageClass := &storagev1.StorageClass{}
		err := v.Client.Get(ctx, types.NamespacedName{Name: scName}, storageClass)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return warnings, fmt.Errorf("StorageClass %q does not exist in cluster", scName)
			}
			// Transient error - warn but allow (API server might be temporarily unavailable)
			warnings = append(warnings,
				fmt.Sprintf("Could not verify StorageClass %q exists: %v", scName, err))
		}
	}

	// Validate S3 credentials Secret if configured (warning only, not error)
	if ds.Spec.S3 != nil && ds.Spec.S3.CredentialsSecretRef != nil {
		secretName := ds.Spec.S3.CredentialsSecretRef.SecretName
		secret := &corev1.Secret{}
		err := v.Client.Get(ctx, types.NamespacedName{
			Name:      secretName,
			Namespace: ds.Namespace,
		}, secret)
		if err != nil {
			if apierrors.IsNotFound(err) {
				warnings = append(warnings,
					fmt.Sprintf("S3 credentials Secret %q not found; ensure it exists before DittoFS pod starts", secretName))
			} else {
				warnings = append(warnings,
					fmt.Sprintf("Could not verify S3 credentials Secret %q: %v", secretName, err))
			}
		} else {
			// Secret exists, validate it has required keys
			ref := ds.Spec.S3.CredentialsSecretRef
			accessKeyIDKey := ref.AccessKeyIDKey
			if accessKeyIDKey == "" {
				accessKeyIDKey = "accessKeyId"
			}
			secretAccessKeyKey := ref.SecretAccessKeyKey
			if secretAccessKeyKey == "" {
				secretAccessKeyKey = "secretAccessKey"
			}

			if _, ok := secret.Data[accessKeyIDKey]; !ok {
				warnings = append(warnings,
					fmt.Sprintf("S3 credentials Secret %q missing key %q", secretName, accessKeyIDKey))
			}
			if _, ok := secret.Data[secretAccessKeyKey]; !ok {
				warnings = append(warnings,
					fmt.Sprintf("S3 credentials Secret %q missing key %q", secretName, secretAccessKeyKey))
			}
		}
	}

	// Validate Percona configuration
	if ds.Spec.Percona != nil && ds.Spec.Percona.Enabled {
		// Check if PerconaPGCluster CRD is installed
		gvk := schema.GroupVersionKind{
			Group:   "pgv2.percona.com",
			Version: "v2",
			Kind:    "PerconaPGCluster",
		}
		_, err := v.Client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return warnings, fmt.Errorf("Percona PostgreSQL Operator CRD not installed; install pg-operator first or disable percona.enabled")
		}

		// Warn if both Percona and PostgresSecretRef are set
		if ds.Spec.Database != nil && ds.Spec.Database.PostgresSecretRef != nil {
			warnings = append(warnings,
				"Both percona.enabled and database.postgresSecretRef are set; Percona-managed PostgreSQL will be used (PostgresSecretRef ignored)")
		}

		// Validate Percona StorageClass if specified
		if ds.Spec.Percona.StorageClassName != nil && *ds.Spec.Percona.StorageClassName != "" {
			scName := *ds.Spec.Percona.StorageClassName
			storageClass := &storagev1.StorageClass{}
			err := v.Client.Get(ctx, types.NamespacedName{Name: scName}, storageClass)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return warnings, fmt.Errorf("Percona StorageClass %q does not exist in cluster", scName)
				}
				warnings = append(warnings,
					fmt.Sprintf("Could not verify Percona StorageClass %q exists: %v", scName, err))
			}
		}

		// Validate backup configuration if enabled
		if ds.Spec.Percona.Backup != nil && ds.Spec.Percona.Backup.Enabled {
			backup := ds.Spec.Percona.Backup

			// Bucket is required if backup enabled
			if backup.Bucket == "" {
				return warnings, fmt.Errorf("percona.backup.bucket is required when backup is enabled")
			}

			// Endpoint is required for S3-compatible storage
			if backup.Endpoint == "" {
				return warnings, fmt.Errorf("percona.backup.endpoint is required when backup is enabled")
			}

			// Warn if credentials secret not specified
			if backup.CredentialsSecretRef == nil {
				warnings = append(warnings,
					"percona.backup.credentialsSecretRef not set; pgBackRest may fail to authenticate with S3")
			} else {
				// Check if credentials secret exists (warning only)
				secret := &corev1.Secret{}
				err := v.Client.Get(ctx, types.NamespacedName{
					Name:      backup.CredentialsSecretRef.Name,
					Namespace: ds.Namespace,
				}, secret)
				if err != nil {
					if apierrors.IsNotFound(err) {
						warnings = append(warnings,
							fmt.Sprintf("pgBackRest credentials Secret %q not found; ensure it exists before PostgreSQL starts", backup.CredentialsSecretRef.Name))
					}
				}
			}
		}
	}

	return warnings, nil
}
