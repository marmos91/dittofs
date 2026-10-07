package common

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// newCappedEngine is newTestEngine with a small local-store cap and a short
// backpressure wait. With no remote, every write stays unsynced, so nothing
// is ever evictable and the cap is reachable.
func newCappedEngine(t *testing.T, cfg journal.Config) *engine.Store {
	t.Helper()

	ms, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open badger metadata store: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	localStore, err := journal.Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("journal.Open failed: %v", err)
	}
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		RemoteSync:     engine.NewRemoteSync(localStore, nil, ms, engine.DefaultConfig()),
		FileChunkStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestWriteToBlockStore_LocalStoreFullIsNoSpace fills the local store through
// the payload choke point the protocol handlers use, then checks the status
// each protocol puts on the wire for the refused write: a write refused for
// space must reach the client as "no space", not as an I/O error.
func TestWriteToBlockStore_LocalStoreFullIsNoSpace(t *testing.T) {
	bs := newCappedEngine(t, journal.Config{
		MaxLocalBytes: 2 << 20,
		SegmentSize:   1 << 20,
		ShardCount:    1,
		EvictMaxWait:  200 * time.Millisecond,
	})
	ctx := context.Background()

	buf := bytes.Repeat([]byte{0xCD}, 256<<10)
	var err error
	for i := range 32 {
		err = WriteToBlockStore(ctx, bs, metadata.PayloadID("full"), buf, uint64(i)*uint64(len(buf)))
		if err != nil {
			break
		}
	}
	if !errors.Is(err, journal.ErrLocalStoreFull) {
		t.Fatalf("writing 8 MiB under a 2 MiB cap returned %v, want an error wrapping journal.ErrLocalStoreFull", err)
	}

	if got := smbtypes.StatusForErr(err); got != smbtypes.StatusDiskFull {
		t.Errorf("SMB status = %v, want STATUS_DISK_FULL", got)
	}
	if got := nfs3types.StatusForErr(err); got != nfs3types.NFS3ErrNoSpc {
		t.Errorf("NFSv3 status = %d, want NFS3ERR_NOSPC (%d)", got, nfs3types.NFS3ErrNoSpc)
	}
	if got := nfs4types.StatusForErr(err); got != nfs4types.NFS4ERR_NOSPC {
		t.Errorf("NFSv4 status = %d, want NFS4ERR_NOSPC (%d)", got, nfs4types.NFS4ERR_NOSPC)
	}
	if got := smbtypes.StatusFor(ClassifyBlockStoreError(err)); got != smbtypes.StatusDiskFull {
		t.Errorf("SMB status through ClassifyBlockStoreError = %v, want STATUS_DISK_FULL", got)
	}
}
