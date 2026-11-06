package durability

import (
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

const rigCap = int64(64 << 10)

var rigIdentity = core.JournalID{0xd, 0xc, 0xa, 0xb}

// testDomains is the file append domain table a test hands the layer: one
// mutex per file, with a count of how many Locks were taken.
type testDomains struct {
	mu    sync.Mutex
	locks map[core.FileID]*sync.Mutex
	taken int
}

func (d *testDomains) Lock(id core.FileID) func() {
	d.mu.Lock()
	m, ok := d.locks[id]
	if !ok {
		if d.locks == nil {
			d.locks = map[core.FileID]*sync.Mutex{}
		}
		m = new(sync.Mutex)
		d.locks[id] = m
	}
	d.taken++
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// rig is a Durability over n real streams on a seam.
type rig struct {
	d       *Durability
	streams *segment.Streams
	seam    *journaltest.Seam
	clock   journaltest.DrivenClock
	domains *testDomains
}

// newRig builds n streams whose sinks are the layer's, and starts the layer.
// The cleanups run in reverse: the layer stops first, then the streams close,
// then the cache shuts down and the pool stops. One rig per test: the
// profiler's watches must not overlap.
func newRig(t *testing.T, n int) *rig {
	t.Helper()
	r := newRigUnstarted(t, n)
	r.d.Start(r.streams)
	return r
}

func newRigUnstarted(t *testing.T, n int) *rig {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(rigCap, nil)
	maker := segment.NewMaker(seam.FS(), set, 1<<62, 0, nil)
	pool := segment.NewPool(maker, segment.Targets{Content: 2 * n}, nil, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	for deadline := time.Now().Add(5 * time.Second); pool.Counters().Spares < int64(2*n); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied its spares: %+v", pool.Counters())
		}
	}
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	clock := journaltest.NewStopwatchClock()
	domains := &testDomains{}
	d := New(Config{Clock: clock, Domains: domains})
	streams := segment.NewStreams(n, rigIdentity, set, pool, cache, d.Sink)
	t.Cleanup(func() { _ = streams.Close() })
	t.Cleanup(d.Stop)
	return &rig{d: d, streams: streams, seam: seam, clock: clock, domains: domains}
}

// file returns a file id distinguished by b.
func file(b byte) core.FileID { return core.NewFileID([16]byte{b}) }

// appendWrite appends one write record of the file and returns its location.
func (r *rig) appendWrite(t *testing.T, f core.FileID, off int64, payload string) segment.Location {
	t.Helper()
	h := format.RecordHeader{
		Kind:    format.KindWrite,
		File:    f,
		Extent:  core.Extent{Off: off, Len: int64(len(payload))},
		Version: core.Version{Counter: uint64(off + 1)},
		Tag:     1,
	}
	p, err := r.streams.For(f).Append(h, format.RecordBody{Payload: []byte(payload), MTime: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return p.Loc
}

// appendKind appends a record of kind k with no payload.
func (r *rig) appendKind(t *testing.T, f core.FileID, k format.Kind, body format.RecordBody) segment.Location {
	t.Helper()
	h := format.RecordHeader{Kind: k, File: f, Extent: core.Extent{Len: int64(len(body.Payload))}, Version: core.Version{Counter: 9}, Tag: 1}
	p, err := r.streams.For(f).Append(h, body)
	if err != nil {
		t.Fatalf("Append of %v: %v", k, err)
	}
	return p.Loc
}

// syncStream syncs the stream the file lives on directly, the way a seal or
// another caller would.
func (r *rig) syncStream(t *testing.T, f core.FileID) {
	t.Helper()
	if err := r.streams.For(f).Sync(); err != nil {
		t.Fatalf("Stream.Sync: %v", err)
	}
}

// ledgerLen returns how many entries the stream's ledger holds.
func (r *rig) ledgerLen(no int) int {
	st := r.d.streams[no]
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.ledger)
}

// eventually polls cond for up to five seconds of real time: a failure
// deadline only, the worker's own goroutine being what it waits on.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
