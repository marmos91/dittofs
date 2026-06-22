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

// Metrics defaults shared by the config generator and the controller so the
// rendered config, container port, and Service always agree.
const (
	// DefaultMetricsPort is the metrics endpoint port when unset.
	DefaultMetricsPort = 9090
	// DefaultMetricsPath is the metrics HTTP path when unset.
	DefaultMetricsPath = "/metrics"
	// MetricsTokenMountPath is where the bearer-token Secret is mounted (read-only)
	// inside the dfs container when BearerTokenSecret is set.
	MetricsTokenMountPath = "/metrics-token"
	// MetricsTokenFileName is the file the bearer token is projected to.
	MetricsTokenFileName = "token"
)

// MetricsEnabled reports whether the metrics endpoint should be turned on.
func (ds *DittoServer) MetricsEnabled() bool {
	return ds.Spec.Metrics != nil && ds.Spec.Metrics.Enabled
}

// MetricsPort returns the metrics endpoint/Service port, applying the default.
func (ds *DittoServer) MetricsPort() int32 {
	if ds.Spec.Metrics != nil && ds.Spec.Metrics.Port > 0 {
		return ds.Spec.Metrics.Port
	}
	return DefaultMetricsPort
}

// MetricsPath returns the metrics HTTP path, applying the default.
func (ds *DittoServer) MetricsPath() string {
	if ds.Spec.Metrics != nil && ds.Spec.Metrics.Path != "" {
		return ds.Spec.Metrics.Path
	}
	return DefaultMetricsPath
}

// MetricsBearerTokenSecret returns the configured bearer-token Secret selector,
// or nil when the endpoint is unauthenticated OR metrics are disabled. Gating on
// MetricsEnabled ensures the token Secret is never mounted/projected into the pod
// when the /metrics listener is off (avoids needlessly exposing the token).
func (ds *DittoServer) MetricsBearerTokenSecret() *corev1.SecretKeySelector {
	if ds.MetricsEnabled() {
		return ds.Spec.Metrics.BearerTokenSecret
	}
	return nil
}

// ServiceMonitorEnabled reports whether a prometheus-operator ServiceMonitor
// has been requested (subject to the CRD-discovery gate in the controller).
func (ds *DittoServer) ServiceMonitorEnabled() bool {
	return ds.MetricsEnabled() && ds.Spec.Metrics.ServiceMonitor != nil &&
		ds.Spec.Metrics.ServiceMonitor.Enabled
}

// MetricsTokenFilePath returns the in-container path the bearer token is mounted
// at, which the rendered metrics.token_file config points to.
func MetricsTokenFilePath() string {
	return MetricsTokenMountPath + "/" + MetricsTokenFileName
}

// Kerberos mount paths shared by the config generator and the controller so the
// rendered kerberos.{keytab_path,krb5_conf} always point at where the operator
// mounts the keytab / krb5.conf Secret keys.
const (
	// KerberosKeytabMountPath is where the keytab Secret is mounted (read-only).
	KerberosKeytabMountPath = "/kerberos"
	// KerberosKeytabFileName is the file the keytab Secret key is projected to.
	KerberosKeytabFileName = "dittofs.keytab"
	// KerberosKrb5ConfMountPath is where the optional krb5.conf Secret is mounted.
	KerberosKrb5ConfMountPath = "/kerberos-krb5"
	// KerberosKrb5ConfFileName is the file the krb5.conf Secret key is projected to.
	KerberosKrb5ConfFileName = "krb5.conf"
)

// KerberosEnabled reports whether the operator should render the kerberos: block
// and mount the keytab into the pod.
func (ds *DittoServer) KerberosEnabled() bool {
	return ds.Spec.Identity != nil && ds.Spec.Identity.Kerberos != nil &&
		ds.Spec.Identity.Kerberos.Enabled
}

// KerberosKeytabSecret returns the keytab Secret selector when Kerberos is
// enabled and a keytab is referenced, otherwise nil.
func (ds *DittoServer) KerberosKeytabSecret() *corev1.SecretKeySelector {
	if !ds.KerberosEnabled() {
		return nil
	}
	return ds.Spec.Identity.Kerberos.KeytabSecretRef
}

// KerberosKrb5ConfSecret returns the krb5.conf Secret selector when Kerberos is
// enabled and one is referenced, otherwise nil.
func (ds *DittoServer) KerberosKrb5ConfSecret() *corev1.SecretKeySelector {
	if !ds.KerberosEnabled() {
		return nil
	}
	return ds.Spec.Identity.Kerberos.Krb5ConfSecretRef
}

// KerberosKeytabFilePath returns the in-container keytab path the rendered
// kerberos.keytab_path points at, matching where the keytab Secret is mounted.
func KerberosKeytabFilePath() string {
	return KerberosKeytabMountPath + "/" + KerberosKeytabFileName
}

// KerberosKrb5ConfFilePath returns the in-container krb5.conf path the rendered
// kerberos.krb5_conf points at when a krb5.conf Secret is mounted.
func KerberosKrb5ConfFilePath() string {
	return KerberosKrb5ConfMountPath + "/" + KerberosKrb5ConfFileName
}
