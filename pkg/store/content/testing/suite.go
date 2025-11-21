package testing

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content"
)

// StoreTestSuite is a comprehensive test suite for ContentStore implementations.
// It tests the interface contract, not implementation details, making it reusable
// across different implementations (memory, filesystem, S3, etc.).
//
// Usage:
//
//	func TestMyContentStore(t *testing.T) {
//	    suite := &testing.StoreTestSuite{
//	        NewStore: func() content.ContentStore {
//	            return mystore.New()
//	        },
//	    }
//	    suite.Run(t)
//	}
type StoreTestSuite struct {
	// NewStore is a factory function that creates a fresh ContentStore instance
	// for each test. This ensures test isolation.
	NewStore func() content.ContentStore
}

// Run executes all tests in the suite.
//
// Note: SeekableOperations tests have been removed because not all backends support seeking.
// Object storage backends (S3, etc.) don't provide seekable readers.
// For backends that do support seeking (filesystem, memory), test it separately in backend-specific tests.
func (suite *StoreTestSuite) Run(t *testing.T) {
	t.Run("BasicOperations", suite.RunBasicTests)
	t.Run("WriteOperations", suite.RunWriteTests)
	t.Run("Statistics", suite.RunStatsTests)
}

// testContext returns a standard test context.
func testContext() context.Context {
	return context.Background()
}
