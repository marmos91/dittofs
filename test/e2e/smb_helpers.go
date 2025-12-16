//go:build e2e

package e2e

import (
	"testing"
)

// runSMBOnAllConfigs runs a test on all SMB configurations
func runSMBOnAllConfigs(t *testing.T, testFunc func(t *testing.T, tc *SMBTestContext)) {
	t.Helper()

	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewSMBTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}

// runSMBOnLocalConfigs runs a test on local configurations only (no S3)
func runSMBOnLocalConfigs(t *testing.T, testFunc func(t *testing.T, tc *SMBTestContext)) {
	t.Helper()

	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewSMBTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}

// runSMBOnConfigsWithLargeFileSupport runs a test on configurations that support
// large file operations efficiently (i.e., local backends + S3 with cache).
func runSMBOnConfigsWithLargeFileSupport(t *testing.T, testFunc func(t *testing.T, tc *SMBTestContext)) {
	t.Helper()

	// Local backends + S3 with cache
	configs := append(LocalConfigurations(), S3CachedConfigurations()...)

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewSMBTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}

// skipSMBIfS3WithoutCache skips the test if running on S3 without cache
// and the file size is too large for efficient operation.
func skipSMBIfS3WithoutCache(t *testing.T, tc *SMBTestContext, sizeBytes int64) {
	t.Helper()

	if tc.Config.ContentStore == ContentS3 && !tc.Config.UseCache && isLargeFileSize(sizeBytes) {
		t.Skipf("Skipping large file test (%d bytes) on S3 without cache - use cached S3 config for large files", sizeBytes)
	}
}
