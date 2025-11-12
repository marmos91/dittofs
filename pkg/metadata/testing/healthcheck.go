package testing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func (suite *StoreTestSuite) RunHealthcheckTests(test *testing.T) {
	test.Run("Healthcheck_Success", suite.TestHealthcheck_Success)
}

// TestHealthcheck_Success verifies that a healthy store passes health checks.
func (suite *StoreTestSuite) TestHealthcheck_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	err := store.Healthcheck(ctx)

	// Assert
	require.NoError(test, err, "Healthy store should pass health check")
}
