package testing

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// StoreTestSuite is a comprehensive test suite for MetadataStore implementations.
// It tests the interface contract, not implementation details, making it reusable
// across different implementations (memory, filesystem, database, etc.).
type StoreTestSuite struct {
	// NewStore is a factory function that creates a fresh MetadataStore instance
	// for each test. This ensures test isolation.
	NewStore func() metadata.MetadataStore
}

// Run executes all tests in the suite.
func (suite *StoreTestSuite) Run(test *testing.T) {
	test.Run("Share", suite.RunShareTests)
	test.Run("Server", suite.RunShareTests)
}
