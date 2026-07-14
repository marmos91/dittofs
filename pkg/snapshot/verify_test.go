package snapshot_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// fakeLocators is a HashLocatorResolver backed by a static map.
type fakeLocators map[block.ContentHash]block.ChunkLocator

func (f fakeLocators) GetLocator(_ context.Context, h block.ContentHash) (block.ChunkLocator, bool, error) {
	loc, ok := f[h]
	return loc, ok, nil
}

// blockLocatorsFor maps every hash in hs to a distinct block-resident locator
// (BlockID = hash string). Post-#1493 durability is block-only, so a resolver
// is required for any hash to be probed at all.
func blockLocatorsFor(hs *block.HashSet) fakeLocators {
	res := make(fakeLocators, hs.Len())
	_ = hs.ForEach(func(h block.ContentHash) error {
		res[h] = block.ChunkLocator{BlockID: h.String()}
		return nil
	})
	return res
}

// seedBlocks PutBlocks a one-byte packed block per hash, keyed to match
// blockLocatorsFor.
func seedBlocks(t *testing.T, rs *remotememory.Store, hs *block.HashSet) {
	t.Helper()
	ctx := context.Background()
	_ = hs.ForEach(func(h block.ContentHash) error {
		if err := rs.PutBlock(ctx, h.String(), bytes.NewReader([]byte{0x01})); err != nil {
			t.Fatalf("seed PutBlock: %v", err)
		}
		return nil
	})
}

// TestVerifyRemoteDurability_BlockResidentProbesBlock: a hash whose locator
// names a packed block is proven durable by probing the block object. A missing
// block → the wrapped ErrChunkNotFound.
func TestVerifyRemoteDurability_BlockResidentProbesBlock(t *testing.T) {
	ctx := context.Background()
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	h := gateHash(7)
	hs := block.NewHashSet(1)
	hs.Add(h)
	loc := block.ChunkLocator{BlockID: "blk-present"}
	res := fakeLocators{h: loc}

	if err := rs.PutBlock(ctx, "blk-present", bytes.NewReader([]byte("packed-block-bytes"))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := snapshot.VerifyRemoteDurability(ctx, res, rs, hs, 4); err != nil {
		t.Fatalf("block-resident present: got %v, want nil", err)
	}

	// Missing block → ErrChunkNotFound.
	res2 := fakeLocators{h: block.ChunkLocator{BlockID: "blk-absent"}}
	err := snapshot.VerifyRemoteDurability(ctx, res2, rs, hs, 4)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("missing block: errors.Is(ErrChunkNotFound)=false; err=%v", err)
	}
}

// TestVerifyRemoteDurability_ContentMismatchFailsWhenExtentKnown: when the
// locator carries a wire extent (WireLength>0), the probe reads the chunk back
// via ReadChunk and recomputes BLAKE3. A plain remote returning wrong-but-
// present bytes (ReadChunk ignores hash on base stores) must fail with
// ErrChunkContentMismatch, not pass as durable. Correct bytes under their true
// hash still pass.
func TestVerifyRemoteDurability_ContentMismatchFailsWhenExtentKnown(t *testing.T) {
	ctx := context.Background()
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	body := []byte("the-actual-stored-bytes")
	trueHash := block.ContentHash(blake3.Sum256(body))
	if err := rs.PutBlock(ctx, "blk", bytes.NewReader(body)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// A hash that does NOT match the stored content.
	claimed := gateHash(42)
	if claimed == trueHash {
		t.Fatal("test setup: claimed hash collided with content hash")
	}
	hs := block.NewHashSet(1)
	hs.Add(claimed)
	res := fakeLocators{claimed: block.ChunkLocator{BlockID: "blk", WireLength: int64(len(body))}}
	err := snapshot.VerifyRemoteDurability(ctx, res, rs, hs, 4)
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("content mismatch: errors.Is(ErrChunkContentMismatch)=false; err=%v", err)
	}

	// Correct content under its true hash passes.
	hs2 := block.NewHashSet(1)
	hs2.Add(trueHash)
	res2 := fakeLocators{trueHash: block.ChunkLocator{BlockID: "blk", WireLength: int64(len(body))}}
	if err := snapshot.VerifyRemoteDurability(ctx, res2, rs, hs2, 4); err != nil {
		t.Fatalf("correct content: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_StandaloneLocatorNotDurable: post-#1493 a hash with
// a STANDALONE locator (BlockID=="") — the pre-flip cas/-only shape — is no
// longer block-resident and is reported not durable (ErrChunkNotFound), never
// probing the block store.
func TestVerifyRemoteDurability_StandaloneLocatorNotDurable(t *testing.T) {
	ctx := context.Background()
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	h := gateHash(9)
	hs := block.NewHashSet(1)
	hs.Add(h)
	res := fakeLocators{h: block.ChunkLocator{}} // standalone (BlockID=="")

	err := snapshot.VerifyRemoteDurability(ctx, res, rs, hs, 4)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("standalone locator: errors.Is(ErrChunkNotFound)=false; err=%v", err)
	}
}

// TestVerifyRemoteDurability_NoLocatorNotDurable: a hash absent from the
// resolver, or a nil resolver, cannot be proven durable → ErrChunkNotFound.
func TestVerifyRemoteDurability_NoLocatorNotDurable(t *testing.T) {
	ctx := context.Background()
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 4)

	// Nil resolver.
	if err := snapshot.VerifyRemoteDurability(ctx, nil, rs, hs, 4); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("nil resolver: want ErrChunkNotFound, got %v", err)
	}
	// Empty resolver (no locator for any hash).
	if err := snapshot.VerifyRemoteDurability(ctx, fakeLocators{}, rs, hs, 4); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("empty resolver: want ErrChunkNotFound, got %v", err)
	}
}

// gateHash returns a deterministic ContentHash seeded by seed so each
// test gets unique, sortable hashes without RNG flakiness.
func gateHash(seed byte) block.ContentHash {
	var h block.ContentHash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// seedManifest returns a HashSet pre-populated with n deterministic hashes.
func seedManifest(t *testing.T, n int) *block.HashSet {
	t.Helper()
	hs := block.NewHashSet(n)
	for i := 0; i < n; i++ {
		hs.Add(gateHash(byte(i + 1)))
	}
	return hs
}

// TestVerifyRemoteDurability_HappyPath: every hash in manifest resolves to a
// present block → returns nil.
func TestVerifyRemoteDurability_HappyPath(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 32)
	seedBlocks(t, rs, hs)

	if err := snapshot.VerifyRemoteDurability(context.Background(), blockLocatorsFor(hs), rs, hs, 4); err != nil {
		t.Fatalf("VerifyRemoteDurability: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_EmptyManifest: empty manifest → returns nil
// without probing.
func TestVerifyRemoteDurability_EmptyManifest(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := block.NewHashSet(0)
	if err := snapshot.VerifyRemoteDurability(context.Background(), nil, rs, hs, 4); err != nil {
		t.Fatalf("empty manifest: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_NilManifest: nil manifest → returns nil.
func TestVerifyRemoteDurability_NilManifest(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	if err := snapshot.VerifyRemoteDurability(context.Background(), nil, rs, nil, 4); err != nil {
		t.Fatalf("nil manifest: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_MissingHashFailFast: at least one hash resolves to
// an absent block → returns wrapped ErrChunkNotFound naming that hash.
func TestVerifyRemoteDurability_MissingHashFailFast(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 16)
	// Seed a block for all but the first hash (deterministic via Sorted()).
	sorted := hs.Sorted()
	missing := sorted[0]
	for _, h := range sorted[1:] {
		if err := rs.PutBlock(context.Background(), h.String(), bytes.NewReader([]byte{0x01})); err != nil {
			t.Fatalf("seed PutBlock: %v", err)
		}
	}

	err := snapshot.VerifyRemoteDurability(context.Background(), blockLocatorsFor(hs), rs, hs, 4)
	if err == nil {
		t.Fatal("missing hash: got nil err, want wrapped ErrChunkNotFound")
	}
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("errors.Is(err, ErrChunkNotFound) = false; err = %v", err)
	}
	if !contains(err.Error(), missing.String()) {
		t.Fatalf("err message %q should name missing hash %s", err.Error(), missing.String())
	}
}

// TestVerifyRemoteDurability_IOErrorPropagates: a non-NotFound block-probe
// error propagates without being wrapped as ErrChunkNotFound.
func TestVerifyRemoteDurability_IOErrorPropagates(t *testing.T) {
	sentinel := errors.New("synthetic-io-failure")
	rs := &errBlockRemote{err: sentinel}

	hs := seedManifest(t, 4)

	err := snapshot.VerifyRemoteDurability(context.Background(), blockLocatorsFor(hs), rs, hs, 2)
	if err == nil {
		t.Fatal("got nil err, want propagated I/O error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false; err = %v", err)
	}
	if errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("err should NOT wrap ErrChunkNotFound; got %v", err)
	}
}

// TestVerifyRemoteDurability_ContextCancelHonored: parent ctx cancelled
// mid-flight → returns ctx error.
func TestVerifyRemoteDurability_ContextCancelHonored(t *testing.T) {
	br := newBlockingRemote()
	hs := seedManifest(t, 16)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- snapshot.VerifyRemoteDurability(ctx, blockLocatorsFor(hs), br, hs, 4)
	}()

	// Wait for some probes to be in-flight, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if br.inFlight() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	// Release any goroutines blocked in GetBlockRange so they observe cancel.
	br.releaseAll()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("VerifyRemoteDurability did not return after cancel")
	}
}

// TestVerifyRemoteDurability_ConcurrencyBound: with concurrency=2 and
// many hashes, never more than 2 probes in flight.
func TestVerifyRemoteDurability_ConcurrencyBound(t *testing.T) {
	const concurrency = 2
	const total = 32

	cr := newCountingRemote(5 * time.Millisecond)
	hs := seedManifest(t, total)

	if err := snapshot.VerifyRemoteDurability(context.Background(), blockLocatorsFor(hs), cr, hs, concurrency); err != nil {
		t.Fatalf("VerifyRemoteDurability: %v", err)
	}

	if got := cr.maxInFlight(); got > concurrency {
		t.Fatalf("max in-flight = %d, want <= %d", got, concurrency)
	}
	if got := cr.totalCalls(); got != total {
		t.Fatalf("total probe calls = %d, want %d", got, total)
	}
}

// TestVerifyRemoteDurability_ConcurrencyDefaultsOnNonPositive: 0 or
// negative concurrency clamps to 1 (safe lower bound) — no deadlock, no
// panic.
func TestVerifyRemoteDurability_ConcurrencyDefaultsOnNonPositive(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 8)
	seedBlocks(t, rs, hs)
	locators := blockLocatorsFor(hs)

	for _, c := range []int{0, -1, -100} {
		c := c
		t.Run(fmt.Sprintf("concurrency=%d", c), func(t *testing.T) {
			if err := snapshot.VerifyRemoteDurability(context.Background(), locators, rs, hs, c); err != nil {
				t.Fatalf("concurrency=%d: got %v, want nil", c, err)
			}
		})
	}
}

// TestVerifyRemoteDurability_FailFastCancelsSiblings: a single miss
// cancels in-flight sibling probes — observed by counting completed
// probes being strictly less than the manifest size.
func TestVerifyRemoteDurability_FailFastCancelsSiblings(t *testing.T) {
	const total = 64
	const concurrency = 4

	hs := seedManifest(t, total)
	missing := hs.Sorted()[0]

	sm := &slowMissingRemote{
		// The block probe keys on BlockID, which blockLocatorsFor sets to the
		// hash string; so mark the missing hash's block ID.
		missing: missing.String(),
		delay:   10 * time.Millisecond,
	}

	err := snapshot.VerifyRemoteDurability(context.Background(), blockLocatorsFor(hs), sm, hs, concurrency)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("got %v, want wrapped ErrChunkNotFound", err)
	}

	completed := sm.completedCalls()
	if completed >= total {
		t.Fatalf("expected fewer than %d probe completions due to cancel; got %d", total, completed)
	}
}

// --- test helpers ---

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// errBlockRemote returns the configured err from every block probe.
type errBlockRemote struct {
	err error
}

func (e *errBlockRemote) PutBlock(context.Context, string, io.Reader) error { return e.err }
func (e *errBlockRemote) GetBlock(context.Context, string) ([]byte, error)  { return nil, e.err }
func (e *errBlockRemote) GetBlockRange(context.Context, string, int64, int64) ([]byte, error) {
	return nil, e.err
}
func (e *errBlockRemote) DeleteBlock(context.Context, string) error { return e.err }
func (e *errBlockRemote) WalkBlocks(context.Context, func(string, block.Meta) error) error {
	return e.err
}

// blockingRemote: GetBlockRange blocks on a per-call gate until release.
type blockingRemote struct {
	release chan struct{}
	flying  atomic.Int64
}

func newBlockingRemote() *blockingRemote {
	return &blockingRemote{release: make(chan struct{})}
}

func (b *blockingRemote) inFlight() int64 { return b.flying.Load() }
func (b *blockingRemote) releaseAll()     { close(b.release) }

func (b *blockingRemote) GetBlockRange(ctx context.Context, _ string, _, _ int64) ([]byte, error) {
	b.flying.Add(1)
	defer b.flying.Add(-1)
	select {
	case <-b.release:
		return []byte{0x01}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockingRemote) PutBlock(context.Context, string, io.Reader) error { return nil }
func (b *blockingRemote) GetBlock(context.Context, string) ([]byte, error)  { return nil, nil }
func (b *blockingRemote) DeleteBlock(context.Context, string) error         { return nil }
func (b *blockingRemote) WalkBlocks(context.Context, func(string, block.Meta) error) error {
	return nil
}

// countingRemote: tracks max-in-flight + total calls; sleeps `delay`
// per probe so the bound assertion is observable.
type countingRemote struct {
	delay  time.Duration
	flying atomic.Int64
	maxFly atomic.Int64
	calls  atomic.Int64
}

func newCountingRemote(delay time.Duration) *countingRemote {
	return &countingRemote{delay: delay}
}

func (c *countingRemote) maxInFlight() int64 { return c.maxFly.Load() }
func (c *countingRemote) totalCalls() int64  { return c.calls.Load() }

func (c *countingRemote) GetBlockRange(ctx context.Context, _ string, _, _ int64) ([]byte, error) {
	c.calls.Add(1)
	now := c.flying.Add(1)
	defer c.flying.Add(-1)
	for {
		prev := c.maxFly.Load()
		if now <= prev {
			break
		}
		if c.maxFly.CompareAndSwap(prev, now) {
			break
		}
	}
	select {
	case <-time.After(c.delay):
		return []byte{0x01}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *countingRemote) PutBlock(context.Context, string, io.Reader) error { return nil }
func (c *countingRemote) GetBlock(context.Context, string) ([]byte, error)  { return nil, nil }
func (c *countingRemote) DeleteBlock(context.Context, string) error         { return nil }
func (c *countingRemote) WalkBlocks(context.Context, func(string, block.Meta) error) error {
	return nil
}

// slowMissingRemote: returns ErrChunkNotFound for a single nominated blockID,
// nil for everything else, with a small delay per call so the fail-fast
// cancellation is observable.
type slowMissingRemote struct {
	missing   string
	delay     time.Duration
	completed atomic.Int64
}

func (s *slowMissingRemote) completedCalls() int64 { return s.completed.Load() }

func (s *slowMissingRemote) GetBlockRange(ctx context.Context, blockID string, _, _ int64) ([]byte, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.completed.Add(1)
	if blockID == s.missing {
		return nil, block.ErrChunkNotFound
	}
	return []byte{0x01}, nil
}

func (s *slowMissingRemote) PutBlock(context.Context, string, io.Reader) error { return nil }
func (s *slowMissingRemote) GetBlock(context.Context, string) ([]byte, error)  { return nil, nil }
func (s *slowMissingRemote) DeleteBlock(context.Context, string) error         { return nil }
func (s *slowMissingRemote) WalkBlocks(context.Context, func(string, block.Meta) error) error {
	return nil
}
