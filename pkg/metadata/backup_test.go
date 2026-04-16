package metadata

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewPayloadIDSet(t *testing.T) {
	s := NewPayloadIDSet()
	require.NotNil(t, s)
	require.Equal(t, 0, s.Len())
}

func TestPayloadIDSetRoundTrip(t *testing.T) {
	s := NewPayloadIDSet()
	s.Add("a")
	s.Add("b")
	s.Add("c")

	require.True(t, s.Contains("a"))
	require.True(t, s.Contains("b"))
	require.True(t, s.Contains("c"))
	require.False(t, s.Contains("z"))
	require.Equal(t, 3, s.Len())
}

func TestPayloadIDSetDedup(t *testing.T) {
	s := NewPayloadIDSet()
	s.Add("dup")
	s.Add("dup")
	require.Equal(t, 1, s.Len())
}

func TestPayloadIDSetNilSafety(t *testing.T) {
	var s PayloadIDSet
	require.False(t, s.Contains("anything"))
	require.Equal(t, 0, s.Len())
}

// Each driver has its own `var _ metadata.Backupable = (*XxxStore)(nil)` so
// interface drift fails the driver build. Sentinel identity and wrapping are
// exercised by driver tests that return and match these sentinels on real
// errors — testing them here would only test the stdlib's errors.Is.
func TestSentinelsNonNil(t *testing.T) {
	for _, s := range []error{
		ErrBackupUnsupported,
		ErrRestoreDestinationNotEmpty,
		ErrRestoreCorrupt,
		ErrSchemaVersionMismatch,
		ErrBackupAborted,
	} {
		require.NotNil(t, s)
	}
}

// Sanity check: the sentinels are distinct values. A regression that aliased
// two sentinels to the same errors.New call would collapse the taxonomy.
func TestSentinelsDistinct(t *testing.T) {
	sentinels := []error{
		ErrBackupUnsupported,
		ErrRestoreDestinationNotEmpty,
		ErrRestoreCorrupt,
		ErrSchemaVersionMismatch,
		ErrBackupAborted,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			require.Falsef(t, errors.Is(a, b), "%q must not alias %q", a, b)
		}
	}
}
