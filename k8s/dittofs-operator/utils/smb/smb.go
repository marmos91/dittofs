package smb

import (
	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
)

// Default SMB configuration values
const (
	DefaultSMBPort                   = 445
	DefaultMaxRequestsPerConnection  = 100
	DefaultMetricsLogInterval        = "5m0s"
	DefaultCreditsStrategy           = "adaptive"
	DefaultCreditsMinGrant           = 16
	DefaultCreditsMaxGrant           = 8192
	DefaultCreditsInitialGrant       = 256
	DefaultCreditsMaxSessionCredits  = 65535
	DefaultCreditsLoadThresholdHigh  = 1000
	DefaultCreditsLoadThresholdLow   = 100
	DefaultAggressiveClientThreshold = 256
)

// GetSMBPort returns the SMB port from the spec or the default.
func GetSMBPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Port != nil {
		return *dittoServer.Spec.SMB.Port
	}
	return DefaultSMBPort
}

// GetMaxConnections returns the max connections from the spec (0 = unlimited).
func GetMaxConnections(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MaxConnections != nil {
		return *dittoServer.Spec.SMB.MaxConnections
	}
	return 0
}

// GetMaxRequestsPerConnection returns the max requests per connection from the spec.
func GetMaxRequestsPerConnection(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MaxRequestsPerConnection != nil {
		return *dittoServer.Spec.SMB.MaxRequestsPerConnection
	}
	return DefaultMaxRequestsPerConnection
}

// getTimeoutValue is a generic helper for extracting timeout values.
func getTimeoutValue(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, accessor func(*dittoiov1alpha1.SMBTimeoutsSpec) string, defaultValue string) string {
	if timeouts != nil {
		if value := accessor(timeouts); value != "" {
			return value
		}
	}
	return defaultValue
}

// GetReadTimeout returns the read timeout from the spec or the default.
func GetReadTimeout(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, defaultValue string) string {
	return getTimeoutValue(timeouts, func(t *dittoiov1alpha1.SMBTimeoutsSpec) string { return t.Read }, defaultValue)
}

// GetWriteTimeout returns the write timeout from the spec or the default.
func GetWriteTimeout(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, defaultValue string) string {
	return getTimeoutValue(timeouts, func(t *dittoiov1alpha1.SMBTimeoutsSpec) string { return t.Write }, defaultValue)
}

// GetIdleTimeout returns the idle timeout from the spec or the default.
func GetIdleTimeout(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, defaultValue string) string {
	return getTimeoutValue(timeouts, func(t *dittoiov1alpha1.SMBTimeoutsSpec) string { return t.Idle }, defaultValue)
}

// GetShutdownTimeout returns the shutdown timeout from the spec or the default.
func GetShutdownTimeout(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, defaultValue string) string {
	return getTimeoutValue(timeouts, func(t *dittoiov1alpha1.SMBTimeoutsSpec) string { return t.Shutdown }, defaultValue)
}

// GetMetricsLogInterval returns the metrics log interval from the spec.
func GetMetricsLogInterval(dittoServer *dittoiov1alpha1.DittoServer) string {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MetricsLogInterval != "" {
		return dittoServer.Spec.SMB.MetricsLogInterval
	}
	return DefaultMetricsLogInterval
}

// getCreditsInt32 is a helper for extracting int32 SMB credits values.
func getCreditsInt32(dittoServer *dittoiov1alpha1.DittoServer, accessor func(*dittoiov1alpha1.SMBCreditsSpec) *int32, defaultValue int32) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil {
		if value := accessor(dittoServer.Spec.SMB.Credits); value != nil {
			return *value
		}
	}
	return defaultValue
}

// GetCreditsStrategy returns the credits strategy from the spec.
func GetCreditsStrategy(dittoServer *dittoiov1alpha1.DittoServer) string {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.Strategy != "" {
		return dittoServer.Spec.SMB.Credits.Strategy
	}
	return DefaultCreditsStrategy
}

// GetCreditsMinGrant returns the minimum credits grant from the spec.
func GetCreditsMinGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.MinGrant }, DefaultCreditsMinGrant)
}

// GetCreditsMaxGrant returns the maximum credits grant from the spec.
func GetCreditsMaxGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.MaxGrant }, DefaultCreditsMaxGrant)
}

// GetCreditsInitialGrant returns the initial credits grant from the spec.
func GetCreditsInitialGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.InitialGrant }, DefaultCreditsInitialGrant)
}

// GetCreditsMaxSessionCredits returns the max session credits from the spec.
func GetCreditsMaxSessionCredits(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.MaxSessionCredits }, DefaultCreditsMaxSessionCredits)
}

// GetCreditsLoadThresholdHigh returns the high load threshold from the spec.
func GetCreditsLoadThresholdHigh(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.LoadThresholdHigh }, DefaultCreditsLoadThresholdHigh)
}

// GetCreditsLoadThresholdLow returns the low load threshold from the spec.
func GetCreditsLoadThresholdLow(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.LoadThresholdLow }, DefaultCreditsLoadThresholdLow)
}

// GetCreditsAggressiveClientThreshold returns the aggressive client threshold from the spec.
func GetCreditsAggressiveClientThreshold(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return getCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.AggressiveClientThreshold }, DefaultAggressiveClientThreshold)
}
