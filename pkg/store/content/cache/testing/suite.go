package testing

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content/cache"
)

// CacheTestSuite is a comprehensive test suite for Cache implementations.
// It tests the interface contract, not implementation details, making it reusable
// across different implementations (memory, filesystem-backed, etc.).
//
// Usage:
//
//	func TestMyCache(t *testing.T) {
//	    suite := &testing.CacheTestSuite{
//	        NewCache: func() cache.Cache {
//	            return mycache.New(1024*1024*100, nil) // 100MB cache
//	        },
//	    }
//	    suite.Run(t)
//	}
type CacheTestSuite struct {
	// NewCache is a factory function that creates a fresh Cache instance
	// for each test. This ensures test isolation.
	NewCache func() cache.Cache
}

// Run executes all tests in the suite.
func (suite *CacheTestSuite) Run(t *testing.T) {
	t.Run("BasicOperations", suite.RunBasicTests)
	t.Run("WriteOperations", suite.RunWriteTests)
	t.Run("ReadOperations", suite.RunReadTests)
	t.Run("Management", suite.RunManagementTests)
}

// testContext returns a standard test context.
func testContext() context.Context {
	return context.Background()
}
