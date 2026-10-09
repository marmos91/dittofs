//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireAccessDenied asserts that err is a permission-denied (EACCES) failure
// and specifically NOT the EIO regression (#1449) where export-gate denials on
// NFSv3 surfaced as "input/output error".
func requireAccessDenied(t *testing.T, err error, what string) {
	t.Helper()
	require.Error(t, err, "%s should be denied", what)
	msg := strings.ToLower(err.Error())
	assert.Contains(t, msg, "permission denied",
		"%s should fail with EACCES, got: %v", what, err)
	assert.NotContains(t, msg, "input/output error",
		"%s must not surface EIO for a permission denial (#1449), got: %v", what, err)
}
