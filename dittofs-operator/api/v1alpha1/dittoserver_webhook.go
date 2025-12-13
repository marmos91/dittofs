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
func (r *DittoServer) validateDittoServer() (admission.Warnings, error) {
	var warnings admission.Warnings

	backendNames := make(map[string]bool)
	for _, backend := range r.Spec.Config.Backends {
		backendNames[backend.Name] = true
	}

	for i, share := range r.Spec.Config.Shares {
		if share.MetadataStore == "" {
			return warnings, fmt.Errorf("share[%d] (%s): metadataStore cannot be empty", i, share.Name)
		}
		if !backendNames[share.MetadataStore] {
			return warnings, fmt.Errorf("share[%d] (%s): metadataStore '%s' does not exist in backends list",
				i, share.Name, share.MetadataStore)
		}

		if share.ContentStore == "" {
			return warnings, fmt.Errorf("share[%d] (%s): contentStore cannot be empty", i, share.Name)
		}
		if !backendNames[share.ContentStore] {
			return warnings, fmt.Errorf("share[%d] (%s): contentStore '%s' does not exist in backends list",
				i, share.Name, share.ContentStore)
		}

		if share.MetadataStore == share.ContentStore {
			warnings = append(warnings, fmt.Sprintf("share[%d] (%s): metadataStore and contentStore reference the same backend '%s' - this may not be optimal",
				i, share.Name, share.MetadataStore))
		}
	}

	for i, share := range r.Spec.Config.Shares {
		for _, backend := range r.Spec.Config.Backends {
			// Check if a metadata store is using an inappropriate backend type
			if backend.Name == share.MetadataStore {
				switch backend.Type {
				case "badger":
					// Good choice for metadata, no warning
				case "s3", "local":
					warnings = append(warnings, fmt.Sprintf("share[%d] (%s): using backend type '%s' for metadata storage is unusual - consider using 'badger' instead",
						i, share.Name, backend.Type))
				default:
					// Unknown backend type - this should have been caught by CRD validation enum
					warnings = append(warnings, fmt.Sprintf("share[%d] (%s): unknown backend type '%s' for metadata storage",
						i, share.Name, backend.Type))
				}
			}
		}
	}

	// Validate no duplicate backend names
	seenBackends := make(map[string]int)
	for i, backend := range r.Spec.Config.Backends {
		if prevIndex, exists := seenBackends[backend.Name]; exists {
			return warnings, fmt.Errorf("duplicate backend name '%s' at indices %d and %d", backend.Name, prevIndex, i)
		}
		seenBackends[backend.Name] = i
	}

	// Validate no duplicate share names
	seenShares := make(map[string]int)
	for i, share := range r.Spec.Config.Shares {
		if prevIndex, exists := seenShares[share.Name]; exists {
			return warnings, fmt.Errorf("duplicate share name '%s' at indices %d and %d", share.Name, prevIndex, i)
		}
		seenShares[share.Name] = i
	}

	// Validate no duplicate export paths
	seenPaths := make(map[string]int)
	for i, share := range r.Spec.Config.Shares {
		if prevIndex, exists := seenPaths[share.ExportPath]; exists {
			return warnings, fmt.Errorf("duplicate export path '%s' at indices %d and %d", share.ExportPath, prevIndex, i)
		}
		seenPaths[share.ExportPath] = i
	}

	return warnings, nil
}
