//go:build e2e

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

// runOnLocalConfigs is a helper that runs a test on local configurations only (no S3)
func runOnLocalConfigs(t *testing.T, testFunc func(t *testing.T, tc *TestContext)) {
	t.Helper()

	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}

// runOnConfigsWithLargeFileSupport is a helper that runs a test on configurations
// that support large file operations efficiently (i.e., local backends + S3 with cache).
// S3 without cache is excluded because large file writes would timeout.
func runOnConfigsWithLargeFileSupport(t *testing.T, testFunc func(t *testing.T, tc *TestContext)) {
	t.Helper()

	// Local backends + S3 with cache
	configs := append(LocalConfigurations(), S3CachedConfigurations()...)

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}

// isLargeFileSize returns true if the file size is considered "large" (>5MB)
// and requires cache for efficient S3 operations
func isLargeFileSize(sizeBytes int64) bool {
	return sizeBytes > 5*1024*1024 // 5MB threshold
}

// skipIfS3WithoutCache skips the test if running on S3 without cache
// and the file size is too large for efficient operation.
// Large files (>5MB) are always skipped on S3 without cache.
func skipIfS3WithoutCache(t *testing.T, tc *TestContext, sizeBytes int64) {
	t.Helper()

	// Only skip if:
	// 1. Content store is S3
	// 2. Cache is not enabled
	// 3. File size is large
	if tc.Config.ContentStore == ContentS3 && !tc.Config.UseCache && isLargeFileSize(sizeBytes) {
		t.Skipf("Skipping large file test (%d bytes) on S3 without cache - use cached S3 config for large files", sizeBytes)
	}
}
