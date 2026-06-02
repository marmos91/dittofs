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
)

// GetManagedJWTSecretName returns the name of the JWT signing-secret the
// operator manages for a DittoServer. It is the single source of truth for the
// managed-secret name shared by GetEffectiveJWTSecretRef and the controller's
// secret reconciliation, so the name cannot drift between the two and silently
// break authentication.
func (ds *DittoServer) GetManagedJWTSecretName() string {
	return ds.Name + "-jwt-secret"
}

// HasUserProvidedJWTSecret returns true if the user explicitly provided a JWT secret reference.
func (ds *DittoServer) HasUserProvidedJWTSecret() bool {
	return ds.Spec.Identity != nil && ds.Spec.Identity.JWT != nil &&
		ds.Spec.Identity.JWT.SecretRef.Name != ""
}

// GetEffectiveJWTSecretRef returns the JWT secret reference to use for a DittoServer.
// If the user provided a secretRef, returns that. Otherwise, returns the managed secret reference.
// This avoids mutating the DittoServer spec while providing consistent access to the JWT secret.
func (ds *DittoServer) GetEffectiveJWTSecretRef() corev1.SecretKeySelector {
	// If user has explicitly provided a JWT secret reference, use that
	if ds.HasUserProvidedJWTSecret() {
		return ds.Spec.Identity.JWT.SecretRef
	}

	// Return the managed secret reference
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: ds.GetManagedJWTSecretName(),
		},
		Key: ManagedJWTSecretKey,
	}
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
//
// The scheme is https:// when spec.controlPlane.tls is set, so the credentials
// the operator sends (admin bootstrap password, operator service-account
// password, bearer tokens) are not transmitted in cleartext over the pod
// network. It defaults to http:// for backward compatibility with control
// planes that do not yet terminate TLS.
func (ds *DittoServer) GetAPIServiceURL() string {
	apiPort := defaultAPIPort
	scheme := "http"
	if ds.Spec.ControlPlane != nil {
		if ds.Spec.ControlPlane.Port > 0 {
			apiPort = int(ds.Spec.ControlPlane.Port)
		}
		if ds.Spec.ControlPlane.TLS {
			scheme = "https"
		}
	}
	return fmt.Sprintf("%s://%s-api.%s.svc.cluster.local:%d", scheme, ds.Name, ds.Namespace, apiPort)
}
