package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newEncryptionProvider builds a local-keyfile KeyProvider for carve/seal tests.
func newEncryptionProvider(t *testing.T) keyprovider.KeyProvider {
	t.Helper()
	raw, err := keyprovider.GenerateKeyFile("carve-test-passphrase")
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "share.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "carve-test-passphrase")
	p, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// carveFixturePayload is the single payload every carveFixture write lands in,
// so Flush/SyncNow(carve) on this ID drains the fixture's dirty ranges.
const carveFixturePayload = "share/p1"

// carveFixture wires a journal-backed *fs.FSStore, a memory metadata store (the
// blockCommitter: Transactor + SyncedHashStore), and the
// provided block-keyed remote into a Syncer with the carve substrate fully
// active (ManualSync — no background dispatcher racing assertions). carveBytes
// sizes the block target.
type carveFixture struct {
	local  *fs.FSStore
	ms     *metadatamemory.MemoryMetadataStore
	remote remote.RemoteBlockStore
	syncer *Syncer
	off    int64 // running write offset within carveFixturePayload
}

// defaultTestCarveBlockSize is the pack size a fixture takes when the test does
// not care: the journal's own default, so a fixture built with it behaves the
// same as a share built from a config that names no size.
const defaultTestCarveBlockSize int64 = 4 << 20

func newCarveFixture(t *testing.T, rbs remote.RemoteStore, carveBytes int64) *carveFixture {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	// carveBytes reaches the carve loop through the local store: the journal owns
	// the loop, so this is the only route to it.
	local, err := fs.NewWithOptions(t.TempDir(), 0, ms, fs.FSStoreOptions{CarveBlockSize: carveBytes})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.ManualSync = true // explicit carve only; no background goroutine racing assertions

	syncer := NewSyncer(local, rbs, ms, cfg)
	syncer.SetSyncedHashStore(ms)
	rblock, ok := rbs.(remote.RemoteBlockStore)
	if !ok {
		t.Fatalf("remote does not implement RemoteBlockStore")
	}
	syncer.SetRemoteBlockStore(rblock)

	if !syncer.carveActive.Load() {
		t.Fatal("carve substrate should be active after wiring")
	}
	return &carveFixture{local: local, ms: ms, remote: rblock, syncer: syncer}
}

// storeChunk writes data into the local journal at the fixture's running offset
// (all under carveFixturePayload) so a subsequent carve packs it to the remote.
// It returns the plaintext content hash for assertions. Carve itself is driven
// explicitly by the caller via syncer.SyncNow / bs.Flush(carveFixturePayload).
func (f *carveFixture) storeChunk(t *testing.T, ctx context.Context, data []byte) block.ContentHash {
	t.Helper()
	if err := f.local.WriteAt(ctx, carveFixturePayload, f.off, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	f.off += int64(len(data))
	return block.ContentHash(blake3.Sum256(data))
}

// countRemoteBlocks returns the number of blocks/ objects on the memory remote.
func countRemoteBlocks(t *testing.T, ctx context.Context, s *remotememory.Store) int {
	t.Helper()
	n := 0
	if err := s.WalkBlocks(ctx, func(string, block.Meta) error { n++; return nil }); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	return n
}

// latencyRemote wraps the memory remote store with an injected per-GET latency
// and concurrency high-water tracking, for cold-read fetch-concurrency tests.
type latencyRemote struct {
	*remotememory.Store
	getLatency  time.Duration
	gets        atomic.Int64
	puts        atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func newLatencyRemote(getLatency time.Duration) *latencyRemote {
	return &latencyRemote{Store: remotememory.New(), getLatency: getLatency}
}

func (r *latencyRemote) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	r.gets.Add(1)
	cur := r.inFlight.Add(1)
	for {
		mx := r.maxInFlight.Load()
		if cur <= mx || r.maxInFlight.CompareAndSwap(mx, cur) {
			break
		}
	}
	defer r.inFlight.Add(-1)
	if r.getLatency > 0 {
		time.Sleep(r.getLatency)
	}
	return r.Store.ReadChunk(ctx, blockID, offset, length, hash)
}

func (r *latencyRemote) PutBlock(ctx context.Context, blockID string, body io.Reader) error {
	r.puts.Add(1)
	return r.Store.PutBlock(ctx, blockID, body)
}

var (
	_ remote.RemoteStore      = (*latencyRemote)(nil)
	_ remote.RemoteBlockStore = (*latencyRemote)(nil)
	_ remote.ChunkReader      = (*latencyRemote)(nil)
)

// TestCarveBlockSizeReachesTheCarveLoop pins the wiring the fixture's carveBytes
// parameter depends on: the size travels FSStoreOptions -> journal.Config, and
// nothing in the engine's syncer config has a say in it. Without the wiring both
// sizes carve at the journal default and the block counts match.
func TestCarveBlockSizeReachesTheCarveLoop(t *testing.T) {
	const payloadSize = 8 << 20

	carveInto := func(t *testing.T, blockSize int64) int {
		t.Helper()
		ctx := context.Background()
		mem := remotememory.New()
		mem.SetDurable(true)
		fx := newCarveFixture(t, mem, blockSize)
		bs, err := New(BlockStoreConfig{
			Local:           fx.local,
			Remote:          mem,
			Syncer:          fx.syncer,
			FileChunkStore:  fx.ms,
			SyncedHashStore: fx.ms,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = bs.Close() })
		bs.SetRequireDurableCommit(true)

		buf := make([]byte, 64<<10)
		for i := range buf {
			buf[i] = byte(i)
		}
		for written := 0; written < payloadSize; written += len(buf) {
			// Vary the bytes so dedup does not collapse the writes into one chunk.
			buf[0] = byte(written >> 16)
			buf[1] = byte(written >> 8)
			fx.storeChunk(t, ctx, buf)
		}
		if _, err := bs.Flush(ctx, carveFixturePayload); err != nil {
			t.Fatalf("Flush(blockSize=%d): %v", blockSize, err)
		}
		return countRemoteBlocks(t, ctx, mem)
	}

	small := carveInto(t, 1<<20)
	large := carveInto(t, 16<<20)
	if small <= large {
		t.Fatalf("a 1 MiB block size must produce more blocks for the same %d bytes than a 16 MiB one: got %d vs %d",
			payloadSize, small, large)
	}
	if large != 1 {
		t.Errorf("a 16 MiB block size should pack %d bytes into one block, got %d", payloadSize, large)
	}
}
