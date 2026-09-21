package metadata_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// phaseCost is the durable and relaxed metadata commit count charged by one
// phase of the create+write+close sequence.
type phaseCost struct {
	durable int
	relaxed int
}

func (c phaseCost) total() int { return c.durable + c.relaxed }

// measure runs fn with the spy zeroed and reports what it charged.
func measure(spy *transactorSpy, fn func()) phaseCost {
	spy.reset()
	fn()
	return phaseCost{durable: spy.durable, relaxed: spy.relaxed}
}

// TestWave7Phase0_CommitInventory counts the metadata commits charged by
// create, an UNSTABLE write and a FILE_SYNC close, which is the inventory the
// per-op write path question starts from: before asking whether the metadata
// commit, the journal fsync and the NFS round-trip overlap, it is worth knowing
// how many metadata commits the sequence pays for at all.
//
// This measures the metadata tier only. It says nothing about the block-journal
// fsync or the round-trip -- those are not on this path and are not visible to
// this spy. Read a number here as "metadata commits", never as "commits".
//
// The counts are pinned rather than only logged. An inventory nobody asserts is
// a number that drifts between the run that produced it and the run that quotes
// it, and this one exists to be quoted.
func TestWave7Phase0_CommitInventory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeback bool
		// What each phase must charge, established by running it.
		create, write, close phaseCost
	}{
		{
			name:      "default share",
			writeback: false,
			create:    phaseCost{relaxed: 1},
			write:     phaseCost{},
			close:     phaseCost{durable: 1},
		},
		{
			name:      "writeback share",
			writeback: true,
			create:    phaseCost{relaxed: 1},
			write:     phaseCost{},
			close:     phaseCost{relaxed: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, spy, root, authCtx := newWritebackFixture(t)
			svc.SetShareWriteback("/wb", tc.writeback)

			create := measure(spy, func() {
				_, _, err := svc.CreateFile(authCtx, root, "f.txt", &metadata.FileAttr{Mode: 0644})
				require.NoError(t, err)
			})

			handle, err := spy.GetChild(authCtx.Context, root, "f.txt")
			require.NoError(t, err)

			write := measure(spy, func() {
				bufferPendingWrite(t, svc, authCtx, handle, 4096)
			})

			closed := measure(spy, func() {
				flushed, err := svc.FlushPendingWriteForFile(authCtx, handle, true)
				require.NoError(t, err)
				require.True(t, flushed)
			})

			t.Logf("create=%+v write=%+v close=%+v  total=%d metadata commits",
				create, write, closed, create.total()+write.total()+closed.total())

			require.Equal(t, tc.create, create, "CREATE metadata commits")
			require.Equal(t, tc.write, write, "WRITE (UNSTABLE) metadata commits")
			require.Equal(t, tc.close, closed, "CLOSE (FILE_SYNC flush) metadata commits")
		})
	}
}
