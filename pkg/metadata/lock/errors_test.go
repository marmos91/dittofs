package lock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// TestErrorConstructorsCarryDiagnostics pins that the diagnostic parameters each
// constructor accepts actually reach the returned error's message.
func TestErrorConstructorsCarryDiagnostics(t *testing.T) {
	t.Parallel()

	t.Run("deadlock names the blocking owners", func(t *testing.T) {
		err := NewDeadlockError("smb:open:42", []string{"smb:open:7", "nlm:host-b"})
		require.Equal(t, errors.ErrDeadlock, err.Code)
		assert.Equal(t, "smb:open:42", err.Path)
		assert.Contains(t, err.Message, "smb:open:7")
		assert.Contains(t, err.Message, "nlm:host-b")
	})

	t.Run("grace period names the remaining seconds", func(t *testing.T) {
		err := NewGracePeriodError(45)
		require.Equal(t, errors.ErrGracePeriod, err.Code)
		assert.Contains(t, err.Message, "45")
	})

	t.Run("lock limit names the counts", func(t *testing.T) {
		err := NewLockLimitExceededError("per-file", 1024, 1024)
		require.Equal(t, errors.ErrLockLimitExceeded, err.Code)
		assert.Contains(t, err.Message, "per-file")
		assert.Contains(t, err.Message, "1024/1024")
	})
}

// TestRegisterClientReportsAdapterAndLimit pins that the connection-limit
// rejection identifies which adapter hit which limit.
func TestRegisterClientReportsAdapterAndLimit(t *testing.T) {
	t.Parallel()

	config := DefaultConnectionTrackerConfig()
	config.MaxConnectionsPerAdapter = map[string]int{"smb": 3}
	ct := NewConnectionTracker(config)

	for _, clientID := range []string{"client1", "client2", "client3"} {
		require.NoError(t, ct.RegisterClient(clientID, "smb", "10.0.0.1:445", 0))
	}

	err := ct.RegisterClient("client4", "smb", "10.0.0.2:445", 0)
	require.Error(t, err)

	var se *errors.StoreError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, errors.ErrConnectionLimitReached, se.Code)
	assert.Contains(t, se.Message, "smb")
	assert.Contains(t, se.Message, "3")
}
