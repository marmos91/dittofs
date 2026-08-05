// Coverage for the SMB2 WRITE Flags field [MS-SMB2] 2.2.21 / 3.3.5.13.
//
// SMB2_WRITEFLAG_WRITE_THROUGH makes an individual WRITE its own durability
// point, while the default WRITE path deliberately defers the block-store
// commit and the metadata fsync off the ack path. The two paths are therefore
// only distinguishable by the durability calls they make, not by the status
// they return, so these tests count real commits: the metadata store is
// wrapped so that the durable (WithTransaction) and relaxed
// (WithTransactionRelaxed) commit entrypoints are tallied separately, with
// both wrappers delegating to a real memory store.
//
// The block-store half of the pair is the same engine.Store.Flush call FLUSH
// and CLOSE make; it has no injection seam reachable from a handler test, and
// is covered by the block-store conformance suites plus the CLOSE tests for
// error surfacing.
package handlers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// txCountingMetaStore delegates every operation to a real memory metadata
// store while tallying which commit entrypoint the metadata service chose.
// Providing WithTransactionRelaxed is what makes the two countable: the plain
// memory store does not implement metadata.RelaxedTransactor, so without this
// wrapper the relaxed path silently collapses onto WithTransaction.
type txCountingMetaStore struct {
	*metamemory.MemoryMetadataStore
	durable atomic.Int64
	relaxed atomic.Int64
}

var _ metadata.RelaxedTransactor = (*txCountingMetaStore)(nil)

func (s *txCountingMetaStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	s.durable.Add(1)
	return s.MemoryMetadataStore.WithTransaction(ctx, fn)
}

func (s *txCountingMetaStore) WithTransactionRelaxed(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	s.relaxed.Add(1)
	return s.MemoryMetadataStore.WithTransaction(ctx, fn)
}

// countWriteCommits issues one WRITE with the given Flags over a connection
// negotiated at the given dialect (the zero dialect leaves the connection
// without crypto state) and reports how many durable and relaxed metadata
// commits that single WRITE performed.
func countWriteCommits(t *testing.T, flags uint32, dialect types.Dialect) (status types.Status, durable, relaxed int64) {
	t.Helper()

	store := &txCountingMetaStore{MemoryMetadataStore: metamemory.NewMemoryMetadataStoreWithDefaults()}
	h, smbCtx, _, fileID := setupWriteTestShare(t, store)
	if dialect != 0 {
		smbCtx.ConnCryptoState = &mockCryptoState{dialect: dialect}
	}

	data := []byte("write-through-payload")
	// Only the WRITE under test may contribute to the tally.
	beforeDurable, beforeRelaxed := store.durable.Load(), store.relaxed.Load()

	resp, err := h.Write(smbCtx, &WriteRequest{
		FileID: fileID,
		Length: uint32(len(data)),
		Data:   data,
		Flags:  flags,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return resp.GetStatus(),
		store.durable.Load() - beforeDurable,
		store.relaxed.Load() - beforeRelaxed
}

// writeCommitDelta reports how many more durable and relaxed metadata commits a
// WRITE carrying the given flags performs than an otherwise identical WRITE
// carrying none.
//
// Measuring a difference rather than an absolute count is what keeps this
// pinned to the durability decision: both runs drive the same handler over the
// same fixture and differ only in the flag, so any metadata write the WRITE
// path performs for its own reasons — file atime, parent-directory atime — lands
// in both tallies and cancels. Should that surrounding traffic ever start or
// stop routing through the transaction entrypoints, the delta is unaffected.
func writeCommitDelta(t *testing.T, flags uint32, dialect types.Dialect) (durable, relaxed int64) {
	t.Helper()

	_, baseDurable, baseRelaxed := countWriteCommits(t, 0, dialect)
	status, gotDurable, gotRelaxed := countWriteCommits(t, flags, dialect)
	if status != types.StatusSuccess {
		t.Fatalf("WRITE(flags=0x%X) status = 0x%08X, want STATUS_SUCCESS", flags, uint32(status))
	}
	return gotDurable - baseDurable, gotRelaxed - baseRelaxed
}

// TestWrite_WriteThrough_CommitsMetadataDurably proves a write-through WRITE
// takes the strict metadata commit inline, before the response is produced,
// in place of the deferred one the default path takes.
func TestWrite_WriteThrough_CommitsMetadataDurably(t *testing.T) {
	durable, relaxed := writeCommitDelta(t, writeFlagWriteThrough, 0)

	if durable != 1 {
		t.Fatalf("write-through added %d durable metadata commit(s) over a plain WRITE, want 1: "+
			"the requested durability point was dropped", durable)
	}
	if relaxed != -1 {
		t.Errorf("write-through changed the relaxed commit count by %d, want -1: the durable "+
			"commit must replace the deferred flush, not run alongside it", relaxed)
	}
}

// TestWrite_Default_StaysDeferred proves the default WRITE path is unchanged:
// no durable commit is added to the ack path.
//
// This one asserts an absolute count rather than a delta on purpose. "Nothing
// on the default ack path fsyncs" is the invariant itself, so anything that
// starts taking the durable entrypoint during a plain WRITE — whatever its
// reason — should fail here. It is also what gives the write-through delta its
// meaning: the baseline it subtracts is a measured zero, not an assumed one.
func TestWrite_Default_StaysDeferred(t *testing.T) {
	status, durable, relaxed := countWriteCommits(t, 0, 0)

	if status != types.StatusSuccess {
		t.Fatalf("default WRITE status = 0x%08X, want STATUS_SUCCESS", uint32(status))
	}
	if durable != 0 {
		t.Errorf("default WRITE performed %d durable metadata commit(s); the deferred "+
			"ack path must not fsync", durable)
	}
	if relaxed == 0 {
		t.Errorf("default WRITE performed no relaxed metadata commit; expected a deferred flush")
	}
}

// TestWrite_UnbufferedAlone_StaysDeferred pins WRITE_UNBUFFERED as a caching
// hint, not a durability request: on its own it must not pull the strict
// commit onto the ack path.
func TestWrite_UnbufferedAlone_StaysDeferred(t *testing.T) {
	durable, relaxed := writeCommitDelta(t, writeFlagWriteUnbuffered, 0)

	if durable != 0 || relaxed != 0 {
		t.Errorf("unbuffered-only WRITE shifted the commit tally by durable=%+d relaxed=%+d; "+
			"want no change from a plain WRITE", durable, relaxed)
	}
}

// TestWrite_WriteThrough_IgnoredOnDialect202 pins that the bit is undefined on
// SMB 2.0.2 and is ignored there rather than forcing a commit.
func TestWrite_WriteThrough_IgnoredOnDialect202(t *testing.T) {
	durable, relaxed := writeCommitDelta(t, writeFlagWriteThrough, types.Dialect0202)

	if durable != 0 || relaxed != 0 {
		t.Errorf("2.0.2 write-through shifted the commit tally by durable=%+d relaxed=%+d; the "+
			"bit is undefined on that dialect and must be ignored", durable, relaxed)
	}
}

// TestPipeWrite_WriteThroughFlags covers the named-pipe rule: a server that
// implements the SMB 3.x dialect family MUST reject WRITE_THROUGH and
// WRITE_UNBUFFERED on a pipe, while a server capped below 3.0 keeps ignoring
// them.
func TestPipeWrite_WriteThroughFlags(t *testing.T) {
	cases := []struct {
		name       string
		maxDialect types.Dialect
		flags      uint32
		wantReject bool
	}{
		{"3.1.1 server rejects write-through", types.Dialect0311, writeFlagWriteThrough, true},
		{"3.1.1 server rejects unbuffered", types.Dialect0311, writeFlagWriteUnbuffered, true},
		{"3.0 server rejects both bits", types.Dialect0300, writeFlagWriteThrough | writeFlagWriteUnbuffered, true},
		{"3.1.1 server accepts no flags", types.Dialect0311, 0, false},
		{"2.1 server ignores write-through", types.Dialect0210, writeFlagWriteThrough, false},
		{"2.0.2 server ignores unbuffered", types.Dialect0202, writeFlagWriteUnbuffered, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, smbCtx, _, _ := setupWriteTestShare(t, nil)
			h.MaxDialect = tc.maxDialect

			pipeID := [16]byte{9}
			h.StoreOpenFile(&OpenFile{
				FileID:   pipeID,
				IsPipe:   true,
				PipeName: "srvsvc",
				Path:     "srvsvc",
			})

			resp, err := h.Write(smbCtx, &WriteRequest{
				FileID: pipeID,
				Length: 4,
				Data:   []byte("ping"),
				Flags:  tc.flags,
			})
			if err != nil {
				t.Fatalf("Write: %v", err)
			}

			got := resp.GetStatus()
			if tc.wantReject {
				if got != types.StatusInvalidParameter {
					t.Fatalf("status = 0x%08X, want STATUS_INVALID_PARAMETER", uint32(got))
				}
				return
			}
			// The flags must not be the reason the request fails; anything
			// past this gate (no pipe registered) reports a different status.
			if got == types.StatusInvalidParameter {
				t.Fatalf("status = STATUS_INVALID_PARAMETER; the flags must be ignored here")
			}
		})
	}
}
