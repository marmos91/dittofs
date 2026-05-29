package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	memorylocal "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"lukechampine.com/blake3"
)

// errBoomLocalPut is the sentinel error returned by failingPutLocal.
var errBoomLocalPut = errors.New("boom local put")

// failingPutLocal wraps a memory LocalStore and overrides Put so
// inlineFetchOrWait observes a persist-failure on the inline path.
// The release channel gates Put so a concurrent waiter can enter the
// in-flight map BEFORE the first caller's Put returns; this proves the
// waiter receives the same error the inline caller does.
type failingPutLocal struct {
	*memorylocal.MemoryStore
	release chan struct{} // closed by the test to let Put proceed
	puts    atomic.Int32  // number of times Put was called
	entered chan struct{} // closed on the first Put entry (oneShot via sync.Once)
	once    sync.Once
}

func newFailingPutLocal() *failingPutLocal {
	return &failingPutLocal{
		MemoryStore: memorylocal.New(),
		release:     make(chan struct{}),
		entered:     make(chan struct{}),
	}
}

// Put records the call, signals first-entry on `entered` (one shot), waits
// for the test to close `release` (with a safety timeout so a buggy test
// does not wedge the suite), then returns the sentinel error.
func (f *failingPutLocal) Put(_ context.Context, _ blockstore.ContentHash, _ []byte) error {
	f.puts.Add(1)
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
	case <-time.After(5 * time.Second):
	}
	return errBoomLocalPut
}

// newFetchSyncer wires the minimum Syncer surface inlineFetchOrWait
// exercises: local, remoteStore, fileBlockStore, inFlight map, and a
// nil HealthMonitor (so IsRemoteHealthy returns true). Coordinator and
// SyncedHashStore are unused on this path.
func newFetchSyncer(localStore local.LocalStore, rs *remotememory.Store, fbs blockstore.EngineFileBlockStore) *Syncer {
	return &Syncer{
		local:          localStore,
		remoteStore:    rs,
		fileBlockStore: fbs,
		inFlight:       make(map[string]*fetchResult),
		stopCh:         make(chan struct{}),
		config:         DefaultConfig(),
		pendingHashes:  make(map[blockstore.ContentHash]struct{}),
	}
}

// seedFileBlock installs a single FileBlock row covering byte window
// [0, len(data)) under payloadID and seeds the remote store's CAS map
// with the matching bytes. inlineFetchOrWait will then resolve the row,
// pass the verified-read through dispatchRemoteFetch, and reach the
// local.Put step under test.
func seedFileBlock(t *testing.T, fbs *stubFileBlockStore, rs *remotememory.Store, payloadID string, data []byte) (blockstore.ContentHash, *blockstore.FileBlock) {
	t.Helper()
	hash := blockstore.ContentHash(blake3.Sum256(data))
	if err := rs.Put(context.Background(), hash, data); err != nil {
		t.Fatalf("seed remote Put: %v", err)
	}
	fb := &blockstore.FileBlock{
		ID:       fmt.Sprintf("%s/%d", payloadID, 0),
		Hash:     hash,
		DataSize: uint32(len(data)),
		State:    blockstore.BlockStateRemote,
	}
	if err := fbs.Put(context.Background(), fb); err != nil {
		t.Fatalf("seed FileBlock Put: %v", err)
	}
	return hash, fb
}

// TestInlineFetchOrWait_LocalPutError_PropagatesToCaller pins the I-5
// fix: when the local CAS Put fails after a successful remote fetch, the
// caller MUST receive the wrapped error (not a silent success). Previous
// behaviour logged at Warn and returned (data, true, nil), so the bytes
// were never persisted but every consumer treated the call as a hit; the
// next read silently re-fetched from S3 (permanent amplification under
// disk-full / local-IO failure).
func TestInlineFetchOrWait_LocalPutError_PropagatesToCaller(t *testing.T) {
	ctx := context.Background()
	payloadID := "payload-inline-err"
	data := []byte("inline-fetch-payload-bytes-for-persist-failure-test")

	loc := newFailingPutLocal()
	close(loc.release) // no waiter — let Put fail immediately
	rs := remotememory.New()
	fbs := newStubFileBlockStore()
	_, _ = seedFileBlock(t, fbs, rs, payloadID, data)

	m := newFetchSyncer(loc, rs, fbs)

	gotData, downloaded, err := m.inlineFetchOrWait(ctx, payloadID, 0)
	if err == nil {
		t.Fatalf("inlineFetchOrWait returned nil err; want error wrapping %v", errBoomLocalPut)
	}
	if !errors.Is(err, errBoomLocalPut) {
		t.Fatalf("err = %v; want errors.Is(errBoomLocalPut)", err)
	}
	if gotData != nil {
		t.Errorf("data = %v; want nil on persist failure", gotData)
	}
	if downloaded {
		t.Errorf("downloaded = true; want false on persist failure (caller must not treat unpersisted bytes as a hit)")
	}

	// The in-flight entry MUST be cleared so the next retry triggers a
	// fresh fetch instead of immediately replaying the same error.
	m.inFlightMu.Lock()
	_, leaked := m.inFlight[inFlightKey(payloadID, 0)]
	m.inFlightMu.Unlock()
	if leaked {
		t.Errorf("inFlight entry leaked for %s/0; want no entry after error return", payloadID)
	}
}

// TestInlineFetchOrWait_LocalPutError_PropagatesToWaiter pins the second
// half of the contract: a concurrent waiter that piggybacks on the
// in-flight map MUST receive the same wrapped error. The previous code
// closed the result channel with err=nil, so the waiter (and any other
// blocked goroutine) saw a successful download for bytes that were never
// persisted.
func TestInlineFetchOrWait_LocalPutError_PropagatesToWaiter(t *testing.T) {
	ctx := context.Background()
	payloadID := "payload-waiter-err"
	data := []byte("inline-fetch-waiter-payload-bytes-for-persist-failure-test")

	loc := newFailingPutLocal()
	rs := remotememory.New()
	fbs := newStubFileBlockStore()
	_, _ = seedFileBlock(t, fbs, rs, payloadID, data)

	m := newFetchSyncer(loc, rs, fbs)

	// Goroutine A: enters inlineFetchOrWait first, registers the in-flight
	// entry, and blocks inside local.Put on loc.release.
	type result struct {
		data       []byte
		downloaded bool
		err        error
	}
	chA := make(chan result, 1)
	go func() {
		d, dl, e := m.inlineFetchOrWait(ctx, payloadID, 0)
		chA <- result{d, dl, e}
	}()

	// Wait for A to enter local.Put before launching B; this guarantees
	// B observes the in-flight entry and takes the waiter branch.
	select {
	case <-loc.entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("goroutine A did not reach local.Put within timeout")
	}

	// Goroutine B: enters inlineFetchOrWait while A is blocked, takes
	// the waiter branch, and blocks on <-existing.done.
	chB := make(chan result, 1)
	go func() {
		d, dl, e := m.inlineFetchOrWait(ctx, payloadID, 0)
		chB <- result{d, dl, e}
	}()

	// Brief settle so B reliably enters the waiter branch (it only needs
	// to acquire inFlightMu and read the existing entry; no Put call).
	time.Sleep(50 * time.Millisecond)

	// Release A's Put so it returns errBoomLocalPut. Both A and B must
	// then observe the same error.
	close(loc.release)

	var resA, resB result
	select {
	case resA = <-chA:
	case <-time.After(3 * time.Second):
		t.Fatalf("goroutine A did not complete within timeout")
	}
	select {
	case resB = <-chB:
	case <-time.After(3 * time.Second):
		t.Fatalf("goroutine B did not complete within timeout")
	}

	if !errors.Is(resA.err, errBoomLocalPut) {
		t.Errorf("A.err = %v; want wrapping errBoomLocalPut", resA.err)
	}
	if !errors.Is(resB.err, errBoomLocalPut) {
		t.Errorf("B.err = %v; want wrapping errBoomLocalPut (waiter must see the persist failure)", resB.err)
	}
	if resB.data != nil {
		t.Errorf("B.data = %v; want nil on persist failure", resB.data)
	}

	// Sanity: only ONE Put call fired — the waiter shared the in-flight
	// download rather than re-issuing it.
	if got := loc.puts.Load(); got != 1 {
		t.Errorf("local.Put call count = %d; want exactly 1 (waiter must piggyback, not re-issue)", got)
	}

	// And the in-flight entry must be cleared so the next retry triggers
	// a fresh fetch.
	m.inFlightMu.Lock()
	_, leaked := m.inFlight[inFlightKey(payloadID, 0)]
	m.inFlightMu.Unlock()
	if leaked {
		t.Errorf("inFlight entry leaked for %s/0; want no entry after error return", payloadID)
	}
}
