package metadata

import (
	"context"
	"errors"
	"io"
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

func TestErrBackupUnsupportedIs(t *testing.T) {
	require.True(t, errors.Is(ErrBackupUnsupported, ErrBackupUnsupported))
}

// stubBackupable is a compile-time assertion that the Backupable interface
// shape is stable. If this file fails to compile, the interface drifted.
type stubBackupable struct{}

func (stubBackupable) Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error) {
	return nil, nil
}
func (stubBackupable) Restore(ctx context.Context, r io.Reader) error { return nil }

func TestBackupableInterfaceShape(t *testing.T) {
	var _ Backupable = (*stubBackupable)(nil)
}
