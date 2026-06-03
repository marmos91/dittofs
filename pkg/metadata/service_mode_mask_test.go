package metadata_test

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modeDOSSparseBit mirrors the high-word DOS attribute bit the SMB sparse-file
// FSCTL handler flips via SetAttrs.ModeOrMask / ModeAndNotMask.
const modeDOSSparseBit = uint32(0x200000)

// TestSetFileAttributes_ModeOrMask_ConcurrentSetClear_NoLoss drives two
// goroutines that respectively set and clear the same high-word mode bit via
// the atomic ModeOrMask / ModeAndNotMask masks. Before the fix the SMB
// handlers did a caller-side GetFile -> compute newMode -> SetFileAttributes
// using the full Mode value, so two concurrent flips would race and the loser
// could revert unrelated bits the winner had set. With the mask fields the
// OR/AND-NOT happens inside the store's own read-modify-write, so only the
// targeted bit ever changes and the POSIX permission bits survive intact.
func TestSetFileAttributes_ModeOrMask_ConcurrentSetClear_NoLoss(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "race.txt", &metadata.FileAttr{
		Mode: 0o644,
	})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "race.txt")
	require.NoError(t, err)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m := modeDOSSparseBit
			_, _ = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{ModeOrMask: &m})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m := modeDOSSparseBit
			_, _ = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{ModeAndNotMask: &m})
		}
	}()

	wg.Wait()

	f, err := fx.service.GetFile(context.Background(), handle)
	require.NoError(t, err)
	assert.Equal(t, uint32(0o644), f.Mode&0o777,
		"POSIX permission bits must not be corrupted by concurrent mode-bit flips")
}

// TestSetFileAttributes_ModeOrMask_CannotSmuggleSetid verifies that the
// ModeOrMask / ModeAndNotMask fields cannot be used to set POSIX permission or
// setid bits, bypassing the SUID/SGID stripping in SetFileAttributes. The masks
// are whitelisted down to the high-word DOS attribute bits before being applied.
func TestSetFileAttributes_ModeOrMask_CannotSmuggleSetid(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "smuggle.txt", &metadata.FileAttr{
		Mode: 0o644,
	})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "smuggle.txt")
	require.NoError(t, err)

	// Attempt to OR in SGID (0o2000), SUID (0o4000) and extra perm bits (0o777)
	// alongside a legitimate DOS attribute bit. Only the DOS bit must take.
	m := modeDOSSparseBit | 0o4000 | 0o2000 | 0o777
	_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{ModeOrMask: &m})
	require.NoError(t, err)

	f, err := fx.service.GetFile(context.Background(), handle)
	require.NoError(t, err)
	assert.Equal(t, modeDOSSparseBit, f.Mode&modeDOSSparseBit, "DOS sparse bit should be set via the mask")
	assert.Zero(t, f.Mode&0o4000, "SUID must not be settable via ModeOrMask")
	assert.Zero(t, f.Mode&0o2000, "SGID must not be settable via ModeOrMask")
	assert.Equal(t, uint32(0o644), f.Mode&0o777, "POSIX permission bits must not be altered via ModeOrMask")
}
