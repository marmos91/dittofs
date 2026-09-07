package metadata_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The commit *count* per file operation is the only lever left on the postgres
// per-commit WAL fsync: the fsync cost of a commit that promised durability
// cannot be removed without weakening the promise, so a win has to come from
// firing fewer such commits. These tests pin how many each leg of a
// create + UNSTABLE WRITE + COMMIT sequence actually fires, so a claim that
// several commits are available to collapse into one is checked against the
// write path rather than asserted.
//
// The counts below are the whole answer: no leg fires more than one commit, so
// there is no intra-operation batch to collapse. What remains is one commit per
// distinct protocol RPC, which the store layer cannot merge because the RPCs
// arrive separately and each must be acked before the next is sent.

// TestCommitCount_DeferredWritePathFiresOneRelaxedCommitPerLeg pins the default
// posture (deferred commits on, no writeback tier): the namespace create and
// the deferrable flush each commit exactly once through the relaxed path, and a
// buffered UNSTABLE WRITE commits not at all.
func TestCommitCount_DeferredWritePathFiresOneRelaxedCommitPerLeg(t *testing.T) {
	svc, spy, root, authCtx := newWritebackFixture(t)

	spy.reset()
	handle := createChild(t, svc, spy, authCtx, root, "f.txt")
	createDurable, createRelaxed := spy.durable, spy.relaxed

	spy.reset()
	bufferPendingWrite(t, svc, authCtx, handle, 4096)
	writeDurable, writeRelaxed := spy.durable, spy.relaxed

	spy.reset()
	flushed, err := svc.FlushPendingWriteForFile(authCtx, handle, false) // deferrable ack
	require.NoError(t, err)
	require.True(t, flushed)
	flushDurable, flushRelaxed := spy.durable, spy.relaxed

	require.Equal(t, 0, createDurable, "create is a pure namespace op: nothing data-paired to fsync")
	require.Equal(t, 1, createRelaxed, "create must fire exactly one relaxed commit")

	require.Equal(t, 0, writeDurable, "a buffered UNSTABLE WRITE must not commit")
	require.Equal(t, 0, writeRelaxed, "a buffered UNSTABLE WRITE must not commit")

	require.Equal(t, 0, flushDurable, "a deferrable flush must not take the inline durable path")
	require.Equal(t, 1, flushRelaxed, "a deferrable flush must fire exactly one relaxed commit")
}

// TestCommitCount_DurableFlushFiresOneDurableCommit pins the FILE_SYNC leg: the
// one commit whose fsync a client was actually promised. It is a single commit,
// so there is nothing to coalesce with it inside the operation; collapsing it
// with the commits of neighbouring RPCs would move the fsync behind an ack that
// already claimed the metadata was stable.
func TestCommitCount_DurableFlushFiresOneDurableCommit(t *testing.T) {
	svc, spy, root, authCtx := newWritebackFixture(t)

	handle := createChild(t, svc, spy, authCtx, root, "f.txt")
	bufferPendingWrite(t, svc, authCtx, handle, 4096)

	spy.reset()
	flushed, err := svc.FlushPendingWriteForFile(authCtx, handle, true) // FILE_SYNC
	require.NoError(t, err)
	require.True(t, flushed)

	require.Equal(t, 1, spy.durable, "a FILE_SYNC flush must fire exactly one durable commit")
	require.Equal(t, 0, spy.relaxed, "a FILE_SYNC flush must not take the relaxed path")
}
