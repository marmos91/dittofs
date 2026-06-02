//go:build integration

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/block/remote/s3"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorymeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Integration coverage for the unified mirror-loop syncer against real
// remote backends. The unit suite (engine_test.go, syncer_put_error_test.go
// engine_delete_test.go) exercises Flush wiring + cascade math with stubs
// this file is the end-to-end smoke proving the loop behaves on actual
// blob stores under
//
//  1. Happy path — every locally rolled-up CAS chunk is mirrored to the
//     remote and IsSynced flips to true.
//  2. Put-then-Mark crash window — a remote that fails MarkSynced ONCE
//     after a successful Put surfaces an error from Flush; the next
//     Flush completes idempotently because Put on identical bytes is a
//     no-op contract per the unified Store surface.
//  3. ListUnsynced snapshot semantics — chunks rolled up mid-Flush land
//     in the NEXT pass, not the current one.
//  4. Refcount cascade — engine.Delete on a fully-synced file drops the
//     synced marker so the synced set stays a strict subset of local CAS.
//
// Two backend fixtures
//
// - memory — pkg/blockstore/remote/memory.New(). Always available.
// - s3 — pkg/blockstore/remote/s3.NewFromConfig against a
//               Localstack/MinIO endpoint. Gated on
// DITTOFS_TEST_S3_ENDPOINT (with DITTOFS_TEST_S3_ACCESS_KEY
// DITTOFS_TEST_S3_SECRET_KEY, DITTOFS_TEST_S3_BUCKET
//               DITTOFS_TEST_S3_REGION, DITTOFS_TEST_S3_FORCE_PATH_STYLE
//               available as overrides). Skipped cleanly when unset.

const (
	envS3Endpoint       = "DITTOFS_TEST_S3_ENDPOINT"
	envS3AccessKey      = "DITTOFS_TEST_S3_ACCESS_KEY"
	envS3SecretKey      = "DITTOFS_TEST_S3_SECRET_KEY"
	envS3Bucket         = "DITTOFS_TEST_S3_BUCKET"
	envS3Region         = "DITTOFS_TEST_S3_REGION"
	envS3ForcePathStyle = "DITTOFS_TEST_S3_FORCE_PATH_STYLE"
)

// remoteBackendFactory describes one of the remote-backend fixtures the
// integration matrix exercises. skip is a per-backend env gate that
// short-circuits the entire matrix row when the backend is unavailable
// in the current environment.
type remoteBackendFactory struct {
	name string
	new  func(t *testing.T) remote.RemoteStore
	skip func(t *testing.T)
}

func integrationBackends() []remoteBackendFactory {
	return []remoteBackendFactory{
		{
			name: "memory",
			new:  newMemoryRemote,
			skip: func(*testing.T) {},
		},
		{
			name: "s3-localstack",
			new:  newS3LocalstackRemote,
			skip: skipIfNoLocalstack,
		},
	}
}

// newMemoryRemote constructs an in-memory RemoteStore. t.Cleanup closes
// it so any goroutine state it owns is released.
func newMemoryRemote(t *testing.T) remote.RemoteStore {
	t.Helper()
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })
	return rs
}

// newS3LocalstackRemote constructs an S3-backed RemoteStore against a
// Localstack/MinIO endpoint. Per-test KeyPrefix isolates parallel runs
// t.Cleanup deletes every object the test wrote and closes the client.
func newS3LocalstackRemote(t *testing.T) remote.RemoteStore {
	t.Helper()

	bucket := os.Getenv(envS3Bucket)
	if bucket == "" {
		bucket = "dittofs-mirror-loop"
	}
	region := os.Getenv(envS3Region)
	if region == "" {
		region = "us-east-1"
	}
	forcePathStyle := true
	if v := os.Getenv(envS3ForcePathStyle); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			forcePathStyle = parsed
		}
	}

	// Per-test prefix so two concurrent subtests do not race on the same
	// object set; sanitize t.Name so it forms a valid S3 prefix segment.
	prefix := "mirror-loop/" + sanitizeS3Segment(t.Name()) + "/"

	cfg := remotes3.Config{
		Bucket:         bucket,
		Region:         region,
		Endpoint:       os.Getenv(envS3Endpoint),
		AccessKey:      os.Getenv(envS3AccessKey),
		SecretKey:      os.Getenv(envS3SecretKey),
		KeyPrefix:      prefix,
		ForcePathStyle: forcePathStyle,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := remotes3.NewFromConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("s3 NewFromConfig: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = store.Walk(cctx, func(h block.ContentHash, _ block.Meta) error {
			_ = store.Delete(cctx, h)
			return nil
		})
		_ = store.Close()
	})
	return store
}

// sanitizeS3Segment maps an arbitrary test name into a string safe to
// embed inside an S3 KeyPrefix path segment. Slashes are preserved so
// nested t.Run names compose; all other non-[A-Za-z0-9._-] runes become
// underscores.
func sanitizeS3Segment(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '-', c == '_', c == '/':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func skipIfNoLocalstack(t *testing.T) {
	t.Helper()
	if os.Getenv(envS3Endpoint) == "" {
		t.Skipf("%s not set; skipping s3-localstack matrix row. Set %s + %s + %s to run.",
			envS3Endpoint, envS3Endpoint, envS3AccessKey, envS3SecretKey)
	}
}

// casLocalStore wraps an in-memory local store so the engine sees a
// real ListUnsynced implementation. The memory backend's stock
// ListUnsynced is intentionally an empty-yield iterator (its godoc
// notes it ships only the no-remote-mirror invariant collapse); the
// production FSStore implementation walks every CAS hash and filters
// via the injected SyncedHashStore. This wrapper recreates that
// real semantic so the integration matrix exercises the mirror loop's
// actual data flow rather than a degenerate no-op (the startup +
// periodic seedPendingFromDisk reconcile drive the pending set through
// this iterator).
//
// Snapshot-at-start semantics mirror the production mirror loop: the
// syncer snapshots its in-memory pending-upload set at pass start and
// uploads outside the lock, so a chunk that lands mid-pass surfaces on
// the NEXT pass. The pending set is fed by the onChunkComplete hook the
// embedded MemoryStore fires per freshly-stored chunk (the parallel of
// the FSStore StoreChunk hot path).
type casLocalStore struct {
	*memorylocal.MemoryStore
	synced metadata.SyncedHashStore

	// mirrorHook, when non-nil, fires from inside Get once (deduped by
	// the caller) so a test can race a parallel local.WriteAt against an
	// in-flight mirror pass to assert snapshot-at-start semantics:
	// mirrorOnce calls Get per snapshotted hash, so a write landing here
	// is excluded from the current pass's snapshot and must surface only
	// on the next pass.
	mirrorHook func(yielded block.ContentHash)
}

func newCASLocalStore(synced metadata.SyncedHashStore) *casLocalStore {
	return &casLocalStore{MemoryStore: memorylocal.New(), synced: synced}
}

// Get fires the mirrorHook (if installed) after delegating to the
// embedded store. mirrorOnce calls Get once per hash in its pending-set
// snapshot, so this is the in-pass injection point a test uses to land a
// late write mid-iteration and assert the snapshot-at-start contract.
func (c *casLocalStore) Get(ctx context.Context, h block.ContentHash) ([]byte, error) {
	data, err := c.MemoryStore.Get(ctx, h)
	if c.mirrorHook != nil {
		c.mirrorHook(h)
	}
	return data, err
}

func (c *casLocalStore) ListUnsynced(ctx context.Context) iter.Seq2[block.ContentHash, error] {
	return func(yield func(block.ContentHash, error) bool) {
		if c.synced == nil {
			return
		}
		var snapshot []block.ContentHash
		if err := c.MemoryStore.Walk(ctx, func(h block.ContentHash, _ block.Meta) error {
			snapshot = append(snapshot, h)
			return nil
		}); err != nil {
			var zero block.ContentHash
			yield(zero, fmt.Errorf("snapshot: %w", err))
			return
		}
		for _, h := range snapshot {
			if err := ctx.Err(); err != nil {
				var zero block.ContentHash
				yield(zero, err)
				return
			}
			synced, err := c.synced.IsSynced(ctx, h)
			if err != nil {
				if !yield(h, fmt.Errorf("synced lookup %s: %w", h, err)) {
					return
				}
				continue
			}
			if synced {
				continue
			}
			if !yield(h, nil) {
				return
			}
		}
	}
}

// failingMarkOnceRemote wraps a RemoteStore and is paired with a
// SyncedHashStore wrapper that fails MarkSynced ONCE before passing
// subsequent calls through. The pairing lets the crash-replay scenario
// fail at the Mark boundary AFTER Put succeeded — exactly the gap the
// Put-then-Mark ordering is designed to recover from. The remote itself
// is unmodified; the surface that can fail in this scenario is the
// SyncedHashStore.
//
// We keep the type around for symmetry but the test instead injects
// failure into the SyncedHashStore (markFailingSyncedHashStore below)
// which is the actually-broken-in-crash-replay component.

// markFailingSyncedHashStore wraps a SyncedHashStore and induces
// failOnceErr on the first MarkSynced call. Subsequent calls pass
// through. Used to simulate a crash between Put and MarkSynced.
type markFailingSyncedHashStore struct {
	mu           sync.Mutex
	inner        metadata.SyncedHashStore
	failOnceErr  error
	failOnceUsed bool

	// marks counts MarkSynced calls that succeeded (post-injection).
	marks atomic.Int32
}

func (m *markFailingSyncedHashStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	return m.inner.IsSynced(ctx, hash)
}

func (m *markFailingSyncedHashStore) MarkSynced(ctx context.Context, hash block.ContentHash) error {
	m.mu.Lock()
	if !m.failOnceUsed && m.failOnceErr != nil {
		m.failOnceUsed = true
		m.mu.Unlock()
		return m.failOnceErr
	}
	m.mu.Unlock()
	if err := m.inner.MarkSynced(ctx, hash); err != nil {
		return err
	}
	m.marks.Add(1)
	return nil
}

func (m *markFailingSyncedHashStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	return m.inner.DeleteSynced(ctx, hash)
}

// stubFBS mirrors the engine_test.go stubFileBlockStore — kept here in
// the external test package because engine_test.go's symbol is in
// package engine and inaccessible from package engine_test.
type stubFBS struct {
	mu     sync.Mutex
	blocks map[string]*block.FileBlock
}

func newStubFBS() *stubFBS {
	return &stubFBS{blocks: make(map[string]*block.FileBlock)}
}

func (s *stubFBS) GetByHash(_ context.Context, h block.ContentHash) (*block.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fb := range s.blocks {
		if fb.Hash == h {
			return fb, nil
		}
	}
	return nil, nil
}

func (s *stubFBS) Put(_ context.Context, block *block.FileBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *block
	s.blocks[block.ID] = &cp
	return nil
}

func (s *stubFBS) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocks, id)
	return nil
}

func (s *stubFBS) IncrementRefCount(_ context.Context, _ string) error { return nil }

func (s *stubFBS) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}

func (s *stubFBS) DecrementRefCountAndReap(_ context.Context, id string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocks, id)
	return 0, nil
}

func (s *stubFBS) AddRef(_ context.Context, h block.ContentHash, _ string, _ block.BlockRef) error {
	// bump RefCount on any row indexed by hash.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fb := range s.blocks {
		if fb.Hash == h {
			fb.RefCount++
			return nil
		}
	}
	return block.ErrUnknownHash
}

func (s *stubFBS) ListPending(_ context.Context, _ time.Duration, _ int) ([]*block.FileBlock, error) {
	return nil, nil
}

func (s *stubFBS) GetFileBlock(_ context.Context, id string) (*block.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fb, ok := s.blocks[id]
	if !ok {
		return nil, block.ErrFileBlockNotFound
	}
	return fb, nil
}

func (s *stubFBS) ListFileBlocks(_ context.Context, payloadID string) ([]*block.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := payloadID + "/"
	var out []*block.FileBlock
	for id, fb := range s.blocks {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			out = append(out, fb)
		}
	}
	return out, nil
}

// integrationFixture bundles every component the four mirror-loop
// scenarios touch. Test bodies call seed(payloadID, data) to land bytes
// in the local CAS via the engine's regular WriteAt path (which routes
// through MemoryStore.AppendWrite + synchronous rollup), then exercise
// bs.Flush + assertions.
type integrationFixture struct {
	bs     *engine.Store
	local  *casLocalStore
	remote remote.RemoteStore
	synced metadata.SyncedHashStore
	fbs    *stubFBS
}

func newIntegrationFixture(t *testing.T, rs remote.RemoteStore, synced metadata.SyncedHashStore) *integrationFixture {
	return newIntegrationFixtureWithConfig(t, rs, synced, engine.DefaultConfig())
}

// newIntegrationFixtureWithConfig is newIntegrationFixture with an
// explicit SyncerConfig. The snapshot-semantics scenario uses it to set
// a long UploadInterval so the periodic uploader cannot drain the
// pending set between the two explicit Flush passes the test drives.
func newIntegrationFixtureWithConfig(t *testing.T, rs remote.RemoteStore, synced metadata.SyncedHashStore, cfg engine.SyncerConfig) *integrationFixture {
	t.Helper()
	if synced == nil {
		synced = memorymeta.NewMemoryMetadataStoreWithDefaults()
	}
	local := newCASLocalStore(synced)
	fbs := newStubFBS()

	syncer := engine.NewSyncer(local, rs, fbs, cfg)

	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           local,
		Remote:          rs,
		Syncer:          syncer,
		FileBlockStore:  fbs,
		SyncedHashStore: synced,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	return &integrationFixture{
		bs:     bs,
		local:  local,
		remote: rs,
		synced: synced,
		fbs:    fbs,
	}
}

// seed appends bytes for payloadID at offset 0 via the public engine
// surface so the local rollup pump runs and CAS chunks materialize.
// Returns the BlockRef list discovered by Walk after rollup — useful
// for cascade assertions that need the produced hash set.
func (f *integrationFixture) seed(t *testing.T, payloadID string, data []byte) []block.ContentHash {
	t.Helper()
	ctx := context.Background()
	if _, err := f.bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("seed WriteAt: %v", err)
	}
	hashes := walkHashes(t, f.local.MemoryStore)
	return hashes
}

func walkHashes(t *testing.T, ms *memorylocal.MemoryStore) []block.ContentHash {
	t.Helper()
	ctx := context.Background()
	var out []block.ContentHash
	if err := ms.Walk(ctx, func(h block.ContentHash, _ block.Meta) error {
		out = append(out, h)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}

// remoteHasHash returns true if rs has an object addressed by hash.
// Uses Head rather than Get to avoid pulling bytes off the wire.
func remoteHasHash(t *testing.T, rs remote.RemoteStore, hash block.ContentHash) bool {
	t.Helper()
	_, err := rs.Head(context.Background(), hash)
	if err == nil {
		return true
	}
	if errors.Is(err, block.ErrChunkNotFound) {
		return false
	}
	t.Fatalf("Head(%s): %v", hash, err)
	return false
}

// runIntegrationMatrix drives the four mirror-loop scenarios across
// every available remote backend. Each scenario is responsible for
// constructing its own fixture (some need to inject failure wrappers
// before engine.New runs); this helper only iterates the backend list
// and the t.Run shell.
func runIntegrationMatrix(t *testing.T, scenario func(t *testing.T, backend remoteBackendFactory)) {
	for _, backend := range integrationBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			backend.skip(t)
			scenario(t, backend)
		})
	}
}

// ----------------------------------------------------------------------
// Scenario 1: Mirror-loop happy path.
//
// Five distinct CAS chunks are landed locally, bs.Flush runs, every
// hash ends up in remote (Head returns non-error) and IsSynced flips
// to true for every hash. FlushResult.Finalized is true.
// ----------------------------------------------------------------------

func TestSyncer_MirrorLoop_HappyPath(t *testing.T) {
	runIntegrationMatrix(t, func(t *testing.T, backend remoteBackendFactory) {
		ctx := context.Background()
		rs := backend.new(t)
		f := newIntegrationFixture(t, rs, nil)

		// Five distinct payloads → five distinct chunks. FastCDC over
		// short unique strings produces one chunk each.
		hashes := make([]block.ContentHash, 0, 5)
		for i := 0; i < 5; i++ {
			payloadID := fmt.Sprintf("happy-%d", i)
			data := []byte(fmt.Sprintf("happy-mirror-loop-payload-%d-%s", i,
				"-padding-to-force-distinct-bytes-per-iteration"))
			seeded := f.seed(t, payloadID, data)
			if len(seeded) == 0 {
				t.Fatalf("seed produced zero hashes for payload %d", i)
			}
		}
		hashes = walkHashes(t, f.local.MemoryStore)
		if len(hashes) < 5 {
			t.Fatalf("expected >=5 CAS chunks after seed, got %d", len(hashes))
		}

		res, err := f.bs.Flush(ctx, "happy-0")
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if !res.Finalized {
			t.Fatalf("Flush.Finalized = false; want true on healthy remote")
		}

		for _, h := range hashes {
			if !remoteHasHash(t, rs, h) {
				t.Errorf("remote missing hash %s after happy-path Flush", h)
			}
			synced, err := f.synced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced(%s): %v", h, err)
			}
			if !synced {
				t.Errorf("IsSynced(%s) = false after happy-path Flush; want true", h)
			}
		}
	})
}

// ----------------------------------------------------------------------
// Scenario 2: Put-then-Mark crash-replay window.
//
// A SyncedHashStore wrapper fails MarkSynced ONCE after the remote.Put
// has succeeded. Flush surfaces an error. The second Flush is the happy
// path: remote.Put on idempotent (hash, identical bytes) returns nil
// MarkSynced flips through the wrapper (no longer failing), every hash
// ends up synced. Crucially, the remote bytes match the local bytes —
// the failed Mark did not corrupt anything.
// ----------------------------------------------------------------------

func TestSyncer_MirrorLoop_PutThenMark_CrashReplay(t *testing.T) {
	runIntegrationMatrix(t, func(t *testing.T, backend remoteBackendFactory) {
		ctx := context.Background()
		rs := backend.new(t)

		innerSynced := memorymeta.NewMemoryMetadataStoreWithDefaults()
		injectedErr := errors.New("induced crash between Put and Mark")
		wrapped := &markFailingSyncedHashStore{
			inner:       innerSynced,
			failOnceErr: injectedErr,
		}
		f := newIntegrationFixture(t, rs, wrapped)

		f.seed(t, "crash-replay", []byte("crash-replay-mirror-loop-payload-with-padding-bytes"))
		hashes := walkHashes(t, f.local.MemoryStore)
		if len(hashes) == 0 {
			t.Fatalf("seed produced zero CAS chunks")
		}

		// First Flush — the wrapped Mark fires errOnce on the first
		// (hash, _) and the loop returns the wrapped error. Crucially
		// Put has already landed bytes in the remote at this point
		// crash replay simulates the kill-9 window.
		_, err := f.bs.Flush(ctx, "crash-replay")
		if err == nil {
			t.Fatalf("first Flush returned nil error; expected wrap of injected MarkSynced failure")
		}
		if !errors.Is(err, injectedErr) {
			t.Fatalf("first Flush error = %v; want wrap of %v", err, injectedErr)
		}

		// Bytes are already remote-resident for the first hash that
		// got past Put. Sanity-check at least one hash is in the
		// remote — proves the failure landed on the Mark surface, not
		// the Put surface.
		anyRemote := false
		for _, h := range hashes {
			if remoteHasHash(t, rs, h) {
				anyRemote = true
				break
			}
		}
		if !anyRemote {
			t.Fatalf("no hash present in remote after first Flush; failure landed earlier than Mark window")
		}

		// Second Flush — Put is a no-op on idempotent identical bytes
		// Mark passes through (the wrapper exhausted its failOnce
		// budget), every hash ends up synced. No corruption: every
		// remote object's bytes match what local has.
		res2, err := f.bs.Flush(ctx, "crash-replay")
		if err != nil {
			t.Fatalf("second Flush: %v", err)
		}
		if !res2.Finalized {
			t.Fatalf("second Flush.Finalized = false; want true after replay")
		}

		for _, h := range hashes {
			localBytes, err := f.local.MemoryStore.Get(ctx, h)
			if err != nil {
				t.Fatalf("local Get(%s): %v", h, err)
			}
			remoteBytes, err := rs.Get(ctx, h)
			if err != nil {
				t.Fatalf("remote Get(%s): %v", h, err)
			}
			if string(localBytes) != string(remoteBytes) {
				t.Errorf("hash %s: remote bytes differ from local after crash-replay (corruption)", h)
			}
			synced, err := innerSynced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced(%s): %v", h, err)
			}
			if !synced {
				t.Errorf("IsSynced(%s) = false after crash-replay Flush; want true", h)
			}
		}
	})
}

// ----------------------------------------------------------------------
// Scenario 3: ListUnsynced snapshot semantics.
//
// Seed N=3 CAS chunks. While Flush runs, append a payload that produces
// 2 new chunks (hX, hY). Assert: the first Flush synced exactly the
// original 3; hX and hY are NOT marked synced after the first pass.
// Run a second bs.Flush; hX and hY now show up as synced.
//
// The ListUnsynced wrapper exposes a mirrorHook that fires post-yield
// per hash — we install a hook that, on the first yielded hash, starts
// a goroutine that AppendWrites the new chunks. The new chunks land in
// the CAS while the loop is still iterating its snapshot of the
// PREVIOUS state, proving the snapshot-at-start contract.
// ----------------------------------------------------------------------

func TestSyncer_MirrorLoop_ListUnsyncedSnapshotSemantics(t *testing.T) {
	runIntegrationMatrix(t, func(t *testing.T, backend remoteBackendFactory) {
		ctx := context.Background()
		rs := backend.new(t)
		f := newIntegrationFixture(t, rs, nil)

		// Seed N=3 chunks via three distinct payloads.
		for i := 0; i < 3; i++ {
			payloadID := fmt.Sprintf("snap-base-%d", i)
			data := []byte(fmt.Sprintf("snapshot-semantics-base-payload-%d-padding-bytes", i))
			f.seed(t, payloadID, data)
		}
		initialHashes := walkHashes(t, f.local.MemoryStore)
		initialSet := hashSet(initialHashes)
		if len(initialHashes) < 3 {
			t.Fatalf("expected >=3 initial CAS chunks, got %d", len(initialHashes))
		}

		// Install the mid-iteration hook. On the first yielded hash
		// fire a goroutine that AppendWrites two new payloads and
		// waits for them to roll up. The goroutine is non-blocking
		// against the in-progress Flush.
		var once sync.Once
		hookDone := make(chan struct{})
		f.local.mirrorHook = func(_ block.ContentHash) {
			once.Do(func() {
				go func() {
					defer close(hookDone)
					_, _ = f.bs.WriteAt(ctx, "snap-late-x", nil,
						[]byte("snapshot-semantics-late-payload-X-padding"), 0)
					_, _ = f.bs.WriteAt(ctx, "snap-late-y", nil,
						[]byte("snapshot-semantics-late-payload-Y-padding"), 0)
				}()
			})
		}

		// First pass: snapshots the initialHashes set, mirrors them
		// and ignores the chunks the hook spawned (they don't exist
		// in the captured snapshot).
		res, err := f.bs.Flush(ctx, "snap-base-0")
		if err != nil {
			t.Fatalf("first Flush: %v", err)
		}
		if !res.Finalized {
			t.Fatalf("first Flush.Finalized = false; want true")
		}

		// Wait for the hook's goroutine to land its writes before
		// asserting on the late hashes.
		select {
		case <-hookDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("mid-iteration hook goroutine did not finish in 5s")
		}

		// Every initial hash is synced.
		for h := range initialSet {
			synced, err := f.synced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced(initial %s): %v", h, err)
			}
			if !synced {
				t.Errorf("initial hash %s not synced after first Flush", h)
			}
		}

		// Late hashes are NOT synced after the first Flush — the
		// snapshot-at-start semantic excluded them. Collect the late
		// hash set by diffing the post-hook Walk against the initial
		// set.
		allHashes := walkHashes(t, f.local.MemoryStore)
		lateHashes := diff(allHashes, initialSet)
		if len(lateHashes) == 0 {
			t.Fatalf("hook did not produce any late hashes; got %d total, %d initial", len(allHashes), len(initialHashes))
		}
		for _, h := range lateHashes {
			synced, err := f.synced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced(late %s): %v", h, err)
			}
			if synced {
				t.Errorf("late hash %s marked synced after first Flush; snapshot semantics violated", h)
			}
		}

		// Disable the hook so the second pass doesn't re-trigger.
		f.local.mirrorHook = nil

		// Second pass: picks up the late hashes.
		res2, err := f.bs.Flush(ctx, "snap-late-x")
		if err != nil {
			t.Fatalf("second Flush: %v", err)
		}
		if !res2.Finalized {
			t.Fatalf("second Flush.Finalized = false; want true")
		}
		for _, h := range lateHashes {
			synced, err := f.synced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced(late %s after second flush): %v", h, err)
			}
			if !synced {
				t.Errorf("late hash %s not synced after second Flush; expected snapshot to advance", h)
			}
		}
	})
}

// hashSet builds an O(1)-lookup set from a hash slice.
func hashSet(hashes []block.ContentHash) map[block.ContentHash]struct{} {
	out := make(map[block.ContentHash]struct{}, len(hashes))
	for _, h := range hashes {
		out[h] = struct{}{}
	}
	return out
}

// diff returns the hashes in candidates that are not present in seen.
func diff(candidates []block.ContentHash, seen map[block.ContentHash]struct{}) []block.ContentHash {
	var out []block.ContentHash
	for _, h := range candidates {
		if _, ok := seen[h]; !ok {
			out = append(out, h)
		}
	}
	return out
}

// ----------------------------------------------------------------------
// Scenario 4: Refcount cascade DeleteSynced end-to-end.
//
// Write a file, Flush so its chunks land in remote + synced set, then
// engine.Delete with the produced BlockRef list (refcount → 0 because
// the coordinator is the test refcount fake). Assert: IsSynced flips
// to false for every hash; the synced set is again a strict subset of
// local CAS.
//
// Distinct from the unit cascade test in engine_delete_test.go in that
// it exercises the cascade end-to-end against a real remote backend —
// 's unit suite proves the wiring; this proves the path under
// real Put/MarkSynced state.
// ----------------------------------------------------------------------

func TestEngine_Delete_CascadesDeleteSynced(t *testing.T) {
	runIntegrationMatrix(t, func(t *testing.T, backend remoteBackendFactory) {
		ctx := context.Background()
		rs := backend.new(t)
		synced := memorymeta.NewMemoryMetadataStoreWithDefaults()

		// Build the fixture using the refcount-aware coordinator and
		// the real metadata-backend SyncedHashStore. Custom assembly
		// because newIntegrationFixture doesn't take a coordinator.
		local := newCASLocalStore(synced)
		fbs := newStubFBS()
		coord := newCascadeCoordinator()

		syncer := engine.NewSyncer(local, rs, fbs, engine.DefaultConfig())
		bs, err := engine.New(engine.BlockStoreConfig{
			Local:           local,
			Remote:          rs,
			Syncer:          syncer,
			FileBlockStore:  fbs,
			Coordinator:     coord,
			SyncedHashStore: synced,
		})
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		if err := bs.Start(ctx); err != nil {
			t.Fatalf("engine.Start: %v", err)
		}
		t.Cleanup(func() { _ = bs.Close() })

		payloadID := "cascade-e2e"
		data := []byte("refcount-cascade-end-to-end-payload-padding-bytes-distinct")
		if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}

		hashes := walkHashes(t, local.MemoryStore)
		if len(hashes) == 0 {
			t.Fatalf("WriteAt produced zero CAS chunks")
		}

		// Seed coord with refcount=1 per block ID so the by-ID reap on
		// Delete drops each to 0 and triggers the cascade. The reap is
		// keyed by EXACT ID "{payloadID}/{offset}"; the offsets here match
		// the blockRefs the Delete below passes (i*4096).
		for i, h := range hashes {
			coord.seedBlock(payloadID, uint64(i)*4096, h, 1)
		}

		if _, err := bs.Flush(ctx, payloadID); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		// Pre-conditions: synced set is a strict superset of nothing
		// every produced hash is synced.
		for _, h := range hashes {
			ok, err := synced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced pre-Delete: %v", err)
			}
			if !ok {
				t.Fatalf("hash %s not synced after Flush; cannot test cascade", h)
			}
		}

		// Build BlockRef list matching the produced hashes. Offset IS
		// material: engine.Delete reaps by EXACT ID "{payloadID}/{offset}",
		// and these offsets (i*4096) match the seedBlock bindings above so
		// the by-ID reap resolves each hash and fires the cascade.
		blockRefs := make([]block.BlockRef, 0, len(hashes))
		for i, h := range hashes {
			blockRefs = append(blockRefs, block.BlockRef{
				Hash:   h,
				Offset: uint64(i) * 4096,
				Size:   4096,
			})
		}

		if err := bs.Delete(ctx, payloadID, blockRefs); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		// Cascade fired: synced is the empty set for these hashes.
		for _, h := range hashes {
			ok, err := synced.IsSynced(ctx, h)
			if err != nil {
				t.Fatalf("IsSynced post-Delete: %v", err)
			}
			if ok {
				t.Errorf("hash %s still synced after Delete; cascade did not fire", h)
			}
		}
	})
}

// cascadeCoordinator is a MetadataCoordinator fake driven by a per-hash
// seeded refcount map. DecrementRefCount returns the post-decrement
// count, which engine.Delete consults to decide whether to fire the
// DeleteSynced cascade.
type cascadeCoordinator struct {
	mu     sync.Mutex
	counts map[block.ContentHash]uint32
	// idHash binds the reap-path row ID "{payloadID}/{offset}" to the hash it
	// bookkeeps. The reap path is keyed by EXACT ID (never by hash), so the
	// coordinator translates the row identity back to the hash — exactly what
	// the production runtime does by reading the row before decrementing.
	idHash map[string]block.ContentHash
}

func newCascadeCoordinator() *cascadeCoordinator {
	return &cascadeCoordinator{
		counts: make(map[block.ContentHash]uint32),
		idHash: make(map[string]block.ContentHash),
	}
}

// seedBlock binds the reap-path row ID "{payloadID}/{offset}" to hash and seeds
// the hash's count, so a by-ID reap can resolve and decrement it.
func (c *cascadeCoordinator) seedBlock(payloadID string, offset uint64, hash block.ContentHash, count uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[hash] = count
	c.idHash[fmt.Sprintf("%s/%d", payloadID, offset)] = hash
}

func (c *cascadeCoordinator) IncrementRefCount(_ context.Context, hash block.ContentHash) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[hash]++
	return nil
}

func (c *cascadeCoordinator) DecrementRefCount(_ context.Context, hash block.ContentHash) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.counts[hash]
	if cur == 0 {
		return 0, nil
	}
	cur--
	c.counts[hash] = cur
	return cur, nil
}

func (c *cascadeCoordinator) DecrementRefCountAndReap(_ context.Context, payloadID string, offset uint64) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hash, ok := c.idHash[fmt.Sprintf("%s/%d", payloadID, offset)]
	if !ok {
		return 0, nil
	}
	cur := c.counts[hash]
	if cur == 0 {
		delete(c.counts, hash)
		return 0, nil
	}
	cur--
	if cur == 0 {
		delete(c.counts, hash)
		return 0, nil
	}
	c.counts[hash] = cur
	return cur, nil
}

func (c *cascadeCoordinator) PersistFileBlocks(_ context.Context, _ string, _ []block.BlockRef, _ block.ObjectID) error {
	return nil
}

func (c *cascadeCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.BlockRef, error) {
	return nil, nil
}

func (c *cascadeCoordinator) FindByObjectID(_ context.Context, _ block.ObjectID) ([]block.BlockRef, error) {
	return nil, nil
}

func (c *cascadeCoordinator) GetFileObjectID(_ context.Context, _ string) (block.ObjectID, error) {
	return block.ObjectID{}, nil
}

var _ engine.MetadataCoordinator = (*cascadeCoordinator)(nil)
