package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var dittoserverlog = logf.Log.WithName("dittoserver-resource")

// SetupWebhookWithManager will setup the manager to manage the webhooks
func (r *DittoServer) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
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
		if r.Spec.ControlPlane.Port < 1 || r.Spec.ControlPlane.Port > 65535 {
			return warnings, fmt.Errorf("controlPlane.port must be between 1 and 65535")
		}
	}

	// Validate metrics port range
	if r.Spec.Metrics != nil && r.Spec.Metrics.Port != 0 {
		if r.Spec.Metrics.Port < 1 || r.Spec.Metrics.Port > 65535 {
			return warnings, fmt.Errorf("metrics.port must be between 1 and 65535")
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

	// Get NFS port (default 2049)
	nfsPort := int32(2049)
	if r.Spec.NFSPort != nil {
		nfsPort = *r.Spec.NFSPort
	}

	// Get SMB port (default 445)
	smbPort := int32(445)
	smbEnabled := r.Spec.SMB != nil && r.Spec.SMB.Enabled
	if smbEnabled && r.Spec.SMB.Port != nil {
		smbPort = *r.Spec.SMB.Port
	}

	// Get API port (default 8080)
	apiPort := int32(8080)
	if r.Spec.ControlPlane != nil && r.Spec.ControlPlane.Port > 0 {
		apiPort = r.Spec.ControlPlane.Port
	}

	// Get metrics port (default 9090)
	metricsPort := int32(9090)
	metricsEnabled := r.Spec.Metrics != nil && r.Spec.Metrics.Enabled
	if metricsEnabled && r.Spec.Metrics.Port > 0 {
		metricsPort = r.Spec.Metrics.Port
	}

	// Build port map for uniqueness check
	ports := map[int32]string{
		nfsPort: "nfs",
		apiPort: "api",
	}

	// Check SMB port uniqueness
	if smbEnabled {
		if existing, ok := ports[smbPort]; ok {
			return nil, fmt.Errorf("port %d is used by both %s and smb", smbPort, existing)
		}
		ports[smbPort] = "smb"
	}

	// Check metrics port uniqueness
	if metricsEnabled {
		if existing, ok := ports[metricsPort]; ok {
			return nil, fmt.Errorf("port %d is used by both %s and metrics", metricsPort, existing)
		}
		ports[metricsPort] = "metrics"
	}

	// Warn about privileged ports (< 1024)
	for port, name := range ports {
		if port < 1024 {
			warnings = append(warnings,
				fmt.Sprintf("%s port %d is privileged; may require CAP_NET_BIND_SERVICE capability", name, port))
		}
	}

	return warnings, nil
}
