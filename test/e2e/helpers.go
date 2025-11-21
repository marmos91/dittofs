package e2e

import (
	"testing"
)

// runOnAllConfigs is a helper that runs a test on all configurations
func runOnAllConfigs(t *testing.T, testFunc func(t *testing.T, tc *TestContext)) {
	t.Helper()

	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}
