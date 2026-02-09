package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// ManagedJWTSecretKey is the key used in auto-generated JWT secrets.
const ManagedJWTSecretKey = "jwt-secret"

// GetEffectiveJWTSecretRef returns the JWT secret reference to use for a DittoServer.
// If the user provided a secretRef, returns that. Otherwise, returns the managed secret reference.
// This avoids mutating the DittoServer spec while providing consistent access to the JWT secret.
func (ds *DittoServer) GetEffectiveJWTSecretRef() corev1.SecretKeySelector {
	// If user has explicitly provided a JWT secret reference, use that
	if ds.Spec.Identity != nil && ds.Spec.Identity.JWT != nil &&
		ds.Spec.Identity.JWT.SecretRef.Name != "" {
		return ds.Spec.Identity.JWT.SecretRef
	}

	// Return the managed secret reference
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: ds.Name + "-jwt-secret",
		},
		Key: ManagedJWTSecretKey,
	}
}

// HasUserProvidedJWTSecret returns true if the user explicitly provided a JWT secret reference.
func (ds *DittoServer) HasUserProvidedJWTSecret() bool {
	return ds.Spec.Identity != nil && ds.Spec.Identity.JWT != nil &&
		ds.Spec.Identity.JWT.SecretRef.Name != ""
}
