package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ManagedJWTSecretKey is the key used in auto-generated JWT secrets.
const ManagedJWTSecretKey = "jwt-secret"

const (
	// TLSCertMountPath is where the control-plane server certificate Secret is
	// mounted (read-only) inside the dfs container when native TLS is enabled.
	TLSCertMountPath = "/tls"

	// TLSClientCAMountPath is where the optional client-CA bundle Secret is
	// mounted (read-only) for mutual TLS.
	TLSClientCAMountPath = "/tls-client-ca"

	// Standard kubernetes.io/tls Secret keys (cert-manager defaults).
	TLSCertKey     = "tls.crt"
	TLSKeyKey      = "tls.key"
	TLSClientCAKey = "ca.crt"
)

// NativeTLSEnabled reports whether the operator should make the dfs pod serve
// native (in-pod) TLS — i.e. a server-certificate Secret was named.
func (ds *DittoServer) NativeTLSEnabled() bool {
	return ds.Spec.ControlPlane != nil && ds.Spec.ControlPlane.CertSecretName != ""
}

// MutualTLSEnabled reports whether client-certificate verification (mTLS) is
// requested. A client-CA Secret is only honored alongside a server cert.
func (ds *DittoServer) MutualTLSEnabled() bool {
	return ds.NativeTLSEnabled() && ds.Spec.ControlPlane.ClientCASecretName != ""
}

// TLSCertFilePath / TLSKeyFilePath / TLSClientCAFilePath return the in-container
// paths the rendered config points controlplane.tls.{cert_file,key_file,client_ca}
// at, matching where the Secrets are mounted.
func TLSCertFilePath() string     { return TLSCertMountPath + "/" + TLSCertKey }
func TLSKeyFilePath() string      { return TLSCertMountPath + "/" + TLSKeyKey }
func TLSClientCAFilePath() string { return TLSClientCAMountPath + "/" + TLSClientCAKey }

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
		// Either the explicit scheme opt-in (edge termination) or a native
		// server-cert Secret (in-pod TLS) means the API speaks https.
		if ds.Spec.ControlPlane.TLS || ds.NativeTLSEnabled() {
			scheme = "https"
		}
	}
	return fmt.Sprintf("%s://%s-api.%s.svc.cluster.local:%d", scheme, ds.Name, ds.Namespace, apiPort)
}
