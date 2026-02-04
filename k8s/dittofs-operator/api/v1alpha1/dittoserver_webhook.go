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

	return warnings, nil
}
