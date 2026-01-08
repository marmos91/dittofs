package smb

import (
	dittoiov1alpha1 "github.com/marmos91/dittofs/dittofs-operator/api/v1alpha1"
)

// getSMBPort returns the SMB port from the spec or the default (445)
func GetSMBPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Port != nil {
		return *dittoServer.Spec.SMB.Port
	}
	return 445
}

func GetMaxConnections(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MaxConnections != nil {
		return *dittoServer.Spec.SMB.MaxConnections
	}
	return 0
}

func GetMaxRequestsPerConnection(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MaxRequestsPerConnection != nil {
		return *dittoServer.Spec.SMB.MaxRequestsPerConnection
	}
	return 100
}

func GetTimeout(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, field, defaultValue string) string {
	if timeouts == nil {
		return defaultValue
	}
	switch field {
	case "read":
		if timeouts.Read != "" {
			return timeouts.Read
		}
	case "write":
		if timeouts.Write != "" {
			return timeouts.Write
		}
	case "idle":
		if timeouts.Idle != "" {
			return timeouts.Idle
		}
	case "shutdown":
		if timeouts.Shutdown != "" {
			return timeouts.Shutdown
		}
	}
	return defaultValue
}

func GetMetricsLogInterval(dittoServer *dittoiov1alpha1.DittoServer) string {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MetricsLogInterval != "" {
		return dittoServer.Spec.SMB.MetricsLogInterval
	}
	return "5m0s"
}

// getSMBCreditsInt32 is a generic helper for extracting int32 SMB credits values
func GetCreditsInt32(dittoServer *dittoiov1alpha1.DittoServer, accessor func(*dittoiov1alpha1.SMBCreditsSpec) *int32, defaultValue int32) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil {
		if value := accessor(dittoServer.Spec.SMB.Credits); value != nil {
			return *value
		}
	}
	return defaultValue
}

func GetCreditsStrategy(dittoServer *dittoiov1alpha1.DittoServer) string {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.Strategy != "" {
		return dittoServer.Spec.SMB.Credits.Strategy
	}
	return "adaptive"
}

func GetCreditsMinGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.MinGrant }, 16)
}

func GetCreditsMaxGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.MaxGrant }, 8192)
}

func GetCreditsInitialGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.InitialGrant }, 256)
}

func GetCreditsMaxSessionCredits(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.MaxSessionCredits }, 65535)
}

func GetCreditsLoadThresholdHigh(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.LoadThresholdHigh }, 1000)
}

func GetCreditsLoadThresholdLow(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.LoadThresholdLow }, 100)
}

func GetCreditsAggressiveClientThreshold(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	return GetCreditsInt32(dittoServer, func(c *dittoiov1alpha1.SMBCreditsSpec) *int32 { return c.AggressiveClientThreshold }, 256)
}
