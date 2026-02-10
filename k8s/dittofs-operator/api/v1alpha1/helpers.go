package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ManagedJWTSecretKey is the key used in auto-generated JWT secrets.
const ManagedJWTSecretKey = "jwt-secret"

const (
	// OperatorCredentialsSecretSuffix is the naming convention for the operator credentials Secret.
	OperatorCredentialsSecretSuffix = "-operator-credentials"

	// AdminCredentialsSecretSuffix is the naming convention for the admin bootstrap Secret.
	AdminCredentialsSecretSuffix = "-admin-credentials"

	// OperatorServiceAccountUsername is the fixed username for the operator service account.
	OperatorServiceAccountUsername = "k8s-operator"

	// defaultAPIPort is the default control plane API port.
	defaultHelperAPIPort = 8080
)

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

// GetOperatorCredentialsSecretName returns the name of the operator credentials Secret.
func (ds *DittoServer) GetOperatorCredentialsSecretName() string {
	return ds.Name + OperatorCredentialsSecretSuffix
}

// GetAdminCredentialsSecretName returns the name of the admin credentials Secret.
func (ds *DittoServer) GetAdminCredentialsSecretName() string {
	return ds.Name + AdminCredentialsSecretSuffix
}

// GetAPIServiceURL returns the in-cluster URL for the DittoFS API service.
func (ds *DittoServer) GetAPIServiceURL() string {
	apiPort := defaultHelperAPIPort
	if ds.Spec.ControlPlane != nil && ds.Spec.ControlPlane.Port > 0 {
		apiPort = int(ds.Spec.ControlPlane.Port)
	}
	return fmt.Sprintf("http://%s-api.%s.svc.cluster.local:%d", ds.Name, ds.Namespace, apiPort)
}
