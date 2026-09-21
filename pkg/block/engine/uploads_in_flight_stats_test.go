package engine_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// gatedPutRemote holds every PutBlock open until release is closed, and
// announces the first arrival on entered. Blocking the upload is the only way
// to observe the store while a block is genuinely in flight: a sleep-and-look
// would be racing the carve pass rather than watching it.
type gatedPutRemote struct {
	*remotememory.Store
	entered   chan struct{}
	release   chan struct{}
	closeOnce sync.Once
}

// letGo releases every parked PutBlock. It is idempotent so the test can both
// call it at the point it means to and register it as cleanup: a failed
// assertion otherwise leaves a carve parked in the remote and the store's Close
// waits out the whole flush timeout.
func (r *gatedPutRemote) letGo() { r.closeOnce.Do(func() { close(r.release) }) }

func newGatedPutRemote() *gatedPutRemote {
	return &gatedPutRemote{
		Store:   remotememory.New(),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (r *gatedPutRemote) PutBlock(ctx context.Context, blockID string, body io.Reader) error {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Store.PutBlock(ctx, blockID, body)
}

// TestBlockStatsReportsUploadsInFlight is the regression guard for block stats
// reporting zero queued work while bytes were demonstrably draining. The
// pending-upload field was a literal 0 in the stats builder, so it read zero
// during an upload, after one, and on a store that had never had a remote —
// the number could not distinguish them and an operator watching a drain saw
// "nothing is happening" throughout.
//
// The assertion is deliberately made against the STATS SNAPSHOT rather than
// against the semaphore the number comes from: the snapshot is what the REST
// handler, the CLI table and the metrics gauge all read, and a value that never
// reaches it is worth nothing. Reverting the stats builder to its constant
// makes the in-flight assertion fail on its own line.
func TestBlockStatsReportsUploadsInFlight(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := newGatedPutRemote()
	bs := newEngineWithGatedRemote(t, ms, mem)
	t.Cleanup(mem.letGo)

	const pid = "share/inflight.bin"
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if _, err := bs.WriteAt(ctx, pid, nil, payload, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	flushed := make(chan error, 1)
	go func() {
		_, err := bs.Flush(ctx, pid)
		flushed <- err
	}()

	select {
	case <-mem.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no PutBlock arrived; the carve never reached the remote")
	}

	if n := bs.GetStatsLite().PendingUploads; n <= 0 {
		t.Fatalf("PendingUploads = %d while a block is parked inside PutBlock; "+
			"stats report no upload work behind bytes that are moving", n)
	}

	mem.letGo()
	if err := <-flushed; err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Back to zero once the uploads land: a gauge that only ever rises would be
	// as useless as one that never does.
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := bs.GetStatsLite().PendingUploads
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PendingUploads stuck at %d after the flush returned", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
