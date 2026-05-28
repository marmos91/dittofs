package snapshot_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// gateHash returns a deterministic ContentHash seeded by seed so each
// test gets unique, sortable hashes without RNG flakiness.
func gateHash(seed byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// seedManifest returns a HashSet pre-populated with n deterministic hashes.
func seedManifest(t *testing.T, n int) *blockstore.HashSet {
	t.Helper()
	hs := blockstore.NewHashSet(n)
	for i := 0; i < n; i++ {
		hs.Add(gateHash(byte(i + 1)))
	}
	return hs
}

// putAll Puts every manifest hash with a one-byte body into rs.
func putAll(t *testing.T, rs *remotememory.Store, hs *blockstore.HashSet) {
	t.Helper()
	ctx := context.Background()
	for _, h := range hs.Sorted() {
		if err := rs.Put(ctx, h, []byte{0x01}); err != nil {
			t.Fatalf("seed Put: %v", err)
		}
	}
}

// TestVerifyRemoteDurability_HappyPath: every hash in manifest is present
// on remote → returns nil.
func TestVerifyRemoteDurability_HappyPath(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 32)
	putAll(t, rs, hs)

	if err := snapshot.VerifyRemoteDurability(context.Background(), rs, hs, 4); err != nil {
		t.Fatalf("VerifyRemoteDurability: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_EmptyManifest: empty manifest → returns nil
// without probing.
func TestVerifyRemoteDurability_EmptyManifest(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := blockstore.NewHashSet(0)
	if err := snapshot.VerifyRemoteDurability(context.Background(), rs, hs, 4); err != nil {
		t.Fatalf("empty manifest: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_NilManifest: nil manifest → returns nil.
func TestVerifyRemoteDurability_NilManifest(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	if err := snapshot.VerifyRemoteDurability(context.Background(), rs, nil, 4); err != nil {
		t.Fatalf("nil manifest: got %v, want nil", err)
	}
}

// TestVerifyRemoteDurability_MissingHashFailFast: at least one hash is
// absent → returns wrapped ErrBlockNotFound naming that hash.
func TestVerifyRemoteDurability_MissingHashFailFast(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 16)
	// Seed all but the first hash (deterministic via Sorted()).
	sorted := hs.Sorted()
	missing := sorted[0]
	for _, h := range sorted[1:] {
		if err := rs.Put(context.Background(), h, []byte{0x01}); err != nil {
			t.Fatalf("seed Put: %v", err)
		}
	}

	err := snapshot.VerifyRemoteDurability(context.Background(), rs, hs, 4)
	if err == nil {
		t.Fatal("missing hash: got nil err, want wrapped ErrBlockNotFound")
	}
	if !errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Fatalf("errors.Is(err, ErrBlockNotFound) = false; err = %v", err)
	}
	if !contains(err.Error(), missing.String()) {
		t.Fatalf("err message %q should name missing hash %s", err.Error(), missing.String())
	}
}

// TestVerifyRemoteDurability_IOErrorPropagates: a non-NotFound Head
// error propagates without being wrapped as ErrBlockNotFound.
func TestVerifyRemoteDurability_IOErrorPropagates(t *testing.T) {
	sentinel := errors.New("synthetic-io-failure")
	rs := &errRemote{err: sentinel}

	hs := seedManifest(t, 4)

	err := snapshot.VerifyRemoteDurability(context.Background(), rs, hs, 2)
	if err == nil {
		t.Fatal("got nil err, want propagated I/O error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false; err = %v", err)
	}
	if errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Fatalf("err should NOT wrap ErrBlockNotFound; got %v", err)
	}
}

// TestVerifyRemoteDurability_ContextCancelHonored: parent ctx cancelled
// mid-flight → returns ctx error.
func TestVerifyRemoteDurability_ContextCancelHonored(t *testing.T) {
	// blockingRemote stalls until released; ensures all in-flight Heads
	// are still running when we cancel the parent ctx.
	br := newBlockingRemote()
	hs := seedManifest(t, 16)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- snapshot.VerifyRemoteDurability(ctx, br, hs, 4)
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
	// Release any goroutines blocked in Head so they observe cancel.
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

	if err := snapshot.VerifyRemoteDurability(context.Background(), cr, hs, concurrency); err != nil {
		t.Fatalf("VerifyRemoteDurability: %v", err)
	}

	if got := cr.maxInFlight(); got > concurrency {
		t.Fatalf("max in-flight = %d, want <= %d", got, concurrency)
	}
	if got := cr.totalCalls(); got != total {
		t.Fatalf("total Head calls = %d, want %d", got, total)
	}
}

// TestVerifyRemoteDurability_ConcurrencyDefaultsOnNonPositive: 0 or
// negative concurrency clamps to 1 (safe lower bound) — no deadlock, no
// panic.
func TestVerifyRemoteDurability_ConcurrencyDefaultsOnNonPositive(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })

	hs := seedManifest(t, 8)
	putAll(t, rs, hs)

	for _, c := range []int{0, -1, -100} {
		c := c
		t.Run(fmt.Sprintf("concurrency=%d", c), func(t *testing.T) {
			if err := snapshot.VerifyRemoteDurability(context.Background(), rs, hs, c); err != nil {
				t.Fatalf("concurrency=%d: got %v, want nil", c, err)
			}
		})
	}
}

// TestVerifyRemoteDurability_FailFastCancelsSiblings: a single miss
// cancels in-flight sibling probes — observed by counting completed
// Heads being strictly less than the manifest size.
func TestVerifyRemoteDurability_FailFastCancelsSiblings(t *testing.T) {
	const total = 64
	const concurrency = 4

	// Returns NotFound for the first hash (by Sorted order), nil for the
	// rest; introduces a small delay so the cancellation race is real.
	hs := seedManifest(t, total)
	missing := hs.Sorted()[0]

	sm := &slowMissingRemote{
		missing: missing,
		delay:   10 * time.Millisecond,
	}

	err := snapshot.VerifyRemoteDurability(context.Background(), sm, hs, concurrency)
	if !errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Fatalf("got %v, want wrapped ErrBlockNotFound", err)
	}

	// With cancellation, far fewer than `total` Heads should complete.
	// The worker pool is concurrency, so an upper bound on completed
	// probes is roughly concurrency + a small slack. Total minus a safety
	// margin is the conservative ceiling.
	completed := sm.completedCalls()
	if completed >= total {
		t.Fatalf("expected fewer than %d Head completions due to cancel; got %d", total, completed)
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

// errRemote returns the configured err from every method. Compile-time
// asserted to satisfy remote.RemoteStore.
type errRemote struct {
	err error
}

var _ remote.RemoteStore = (*errRemote)(nil)

func (e *errRemote) Put(context.Context, blockstore.ContentHash, []byte) error {
	return e.err
}
func (e *errRemote) Get(context.Context, blockstore.ContentHash) ([]byte, error) {
	return nil, e.err
}
func (e *errRemote) GetRange(context.Context, blockstore.ContentHash, int64, int64) ([]byte, error) {
	return nil, e.err
}
func (e *errRemote) Delete(context.Context, blockstore.ContentHash) error { return e.err }
func (e *errRemote) Head(context.Context, blockstore.ContentHash) (blockstore.Meta, error) {
	return blockstore.Meta{}, e.err
}
func (e *errRemote) Walk(context.Context, func(blockstore.ContentHash, blockstore.Meta) error) error {
	return e.err
}
func (e *errRemote) ReadBlockVerified(context.Context, blockstore.ContentHash, blockstore.ContentHash) ([]byte, error) {
	return nil, e.err
}
func (e *errRemote) Close() error                              { return nil }
func (e *errRemote) HealthCheck(context.Context) error         { return e.err }
func (e *errRemote) Healthcheck(context.Context) health.Report { return health.Report{} }

// blockingRemote: Head blocks on a per-call gate until release.
type blockingRemote struct {
	release chan struct{}
	flying  atomic.Int64
}

var _ remote.RemoteStore = (*blockingRemote)(nil)

func newBlockingRemote() *blockingRemote {
	return &blockingRemote{release: make(chan struct{})}
}

func (b *blockingRemote) inFlight() int64 { return b.flying.Load() }
func (b *blockingRemote) releaseAll()     { close(b.release) }

func (b *blockingRemote) Head(ctx context.Context, _ blockstore.ContentHash) (blockstore.Meta, error) {
	b.flying.Add(1)
	defer b.flying.Add(-1)
	select {
	case <-b.release:
		return blockstore.Meta{}, nil
	case <-ctx.Done():
		return blockstore.Meta{}, ctx.Err()
	}
}

func (b *blockingRemote) Put(context.Context, blockstore.ContentHash, []byte) error { return nil }
func (b *blockingRemote) Get(context.Context, blockstore.ContentHash) ([]byte, error) {
	return nil, nil
}
func (b *blockingRemote) GetRange(context.Context, blockstore.ContentHash, int64, int64) ([]byte, error) {
	return nil, nil
}
func (b *blockingRemote) Delete(context.Context, blockstore.ContentHash) error { return nil }
func (b *blockingRemote) Walk(context.Context, func(blockstore.ContentHash, blockstore.Meta) error) error {
	return nil
}
func (b *blockingRemote) ReadBlockVerified(context.Context, blockstore.ContentHash, blockstore.ContentHash) ([]byte, error) {
	return nil, nil
}
func (b *blockingRemote) Close() error                              { return nil }
func (b *blockingRemote) HealthCheck(context.Context) error         { return nil }
func (b *blockingRemote) Healthcheck(context.Context) health.Report { return health.Report{} }

// countingRemote: tracks max-in-flight + total calls; sleeps `delay`
// per Head so the bound assertion is observable.
type countingRemote struct {
	delay  time.Duration
	flying atomic.Int64
	maxFly atomic.Int64
	calls  atomic.Int64
}

var _ remote.RemoteStore = (*countingRemote)(nil)

func newCountingRemote(delay time.Duration) *countingRemote {
	return &countingRemote{delay: delay}
}

func (c *countingRemote) maxInFlight() int64 { return c.maxFly.Load() }
func (c *countingRemote) totalCalls() int64  { return c.calls.Load() }

func (c *countingRemote) Head(ctx context.Context, _ blockstore.ContentHash) (blockstore.Meta, error) {
	c.calls.Add(1)
	now := c.flying.Add(1)
	defer c.flying.Add(-1)
	// Update high-water mark.
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
		return blockstore.Meta{}, nil
	case <-ctx.Done():
		return blockstore.Meta{}, ctx.Err()
	}
}

func (c *countingRemote) Put(context.Context, blockstore.ContentHash, []byte) error { return nil }
func (c *countingRemote) Get(context.Context, blockstore.ContentHash) ([]byte, error) {
	return nil, nil
}
func (c *countingRemote) GetRange(context.Context, blockstore.ContentHash, int64, int64) ([]byte, error) {
	return nil, nil
}
func (c *countingRemote) Delete(context.Context, blockstore.ContentHash) error { return nil }
func (c *countingRemote) Walk(context.Context, func(blockstore.ContentHash, blockstore.Meta) error) error {
	return nil
}
func (c *countingRemote) ReadBlockVerified(context.Context, blockstore.ContentHash, blockstore.ContentHash) ([]byte, error) {
	return nil, nil
}
func (c *countingRemote) Close() error                              { return nil }
func (c *countingRemote) HealthCheck(context.Context) error         { return nil }
func (c *countingRemote) Healthcheck(context.Context) health.Report { return health.Report{} }

// slowMissingRemote: returns ErrBlockNotFound for a single
// nominated hash, nil for everything else, with a small delay per call
// so the fail-fast cancellation is observable.
type slowMissingRemote struct {
	missing   blockstore.ContentHash
	delay     time.Duration
	completed atomic.Int64
}

var _ remote.RemoteStore = (*slowMissingRemote)(nil)

func (s *slowMissingRemote) completedCalls() int64 { return s.completed.Load() }

func (s *slowMissingRemote) Head(ctx context.Context, h blockstore.ContentHash) (blockstore.Meta, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return blockstore.Meta{}, ctx.Err()
	}
	s.completed.Add(1)
	if h == s.missing {
		return blockstore.Meta{}, blockstore.ErrBlockNotFound
	}
	return blockstore.Meta{}, nil
}

func (s *slowMissingRemote) Put(context.Context, blockstore.ContentHash, []byte) error { return nil }
func (s *slowMissingRemote) Get(context.Context, blockstore.ContentHash) ([]byte, error) {
	return nil, nil
}
func (s *slowMissingRemote) GetRange(context.Context, blockstore.ContentHash, int64, int64) ([]byte, error) {
	return nil, nil
}
func (s *slowMissingRemote) Delete(context.Context, blockstore.ContentHash) error { return nil }
func (s *slowMissingRemote) Walk(context.Context, func(blockstore.ContentHash, blockstore.Meta) error) error {
	return nil
}
func (s *slowMissingRemote) ReadBlockVerified(context.Context, blockstore.ContentHash, blockstore.ContentHash) ([]byte, error) {
	return nil, nil
}
func (s *slowMissingRemote) Close() error                              { return nil }
func (s *slowMissingRemote) HealthCheck(context.Context) error         { return nil }
func (s *slowMissingRemote) Healthcheck(context.Context) health.Report { return health.Report{} }
