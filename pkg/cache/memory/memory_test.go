package memory

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	cachetest "github.com/marmos91/dittofs/pkg/cache/testing"
)

// TestMemoryCache runs the complete test suite for MemoryCache.
func TestMemoryCache(t *testing.T) {
	suite := &cachetest.CacheTestSuite{
		NewCache: func() cache.Cache {
			// Create cache with 100MB limit for testing
			maxSize := int64(100 * 1024 * 1024)
			return NewMemoryCache(maxSize, nil)
		},
	}

	suite.Run(t)
}

// TestMemoryCacheUnlimited tests MemoryCache with no size limit.
func TestMemoryCacheUnlimited(t *testing.T) {
	suite := &cachetest.CacheTestSuite{
		NewCache: func() cache.Cache {
			// Create cache with no limit (maxSize = 0)
			return NewMemoryCache(0, nil)
		},
	}

	suite.Run(t)
}
