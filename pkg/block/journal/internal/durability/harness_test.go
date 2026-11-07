package durability_test

import (
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// harnessCap is the segment cap of the sets the tests build: small enough to
// fill, large enough that a few hundred loss records fit.
const harnessCap = int64(64 << 10)

// harnessIdentity is the journal identity every test writes under.
var harnessIdentity = core.JournalID{0xd, 0xe, 0xc, 0xa, 0xd, 0x1}

// marked is one call the recording MarkLoss saw.
type marked struct {
	File    core.FileID
	Extent  core.Extent
	Version core.Version
}

// env is a Durability over real streams on a real directory, the way E8 will
// build one, with a MarkLoss that records its calls.
type env struct {
	t        *testing.T
	dir      string
	seam     *journaltest.Seam
	streams  *segment.Streams
	pool     *segment.Pool
	cache    *segment.Descriptors
	counters *index.Counters
	d        *durability.Durability

	mu      sync.Mutex
	marks   []marked
	markErr error           // what MarkLoss returns, nil by default
	delta   index.HeldDelta // the delta MarkLoss returns, zero by default
	noAcct  bool            // leaves Config.Account nil
	accts   []index.HeldDelta
	log     *slog.Logger // handed to the Durability the next newDurability builds
}

// newEnv builds the env over a fresh directory.
func newEnv(t *testing.T) *env {
	t.Helper()
	prof.Watch(t, prof.Options{})
	return buildEnv(t)
}

// newEnvPinned builds the env over a pool of exactly the given spares: the
// journal's maximum is the sum of the targets, so a spare a test takes or a
// roll draws is never made again, and a class that targets nothing is dry
// from the start.
func newEnvPinned(t *testing.T, targets segment.Targets) *env {
	t.Helper()
	prof.Watch(t, prof.Options{})
	max := int64(targets.Content+targets.Headroom) * harnessCap
	if max == 0 {
		max = harnessCap // the maker refuses a zero maximum; the targets still make nothing
	}
	return buildEnvWith(t, targets, max)
}

// buildEnv is newEnv without the leak watch, for a test that builds two envs
// under the one watch it started itself.
func buildEnv(t *testing.T) *env {
	t.Helper()
	return buildEnvWith(t, segment.Targets{Content: 2, Headroom: 1}, math.MaxInt64)
}

// buildEnvWith is buildEnv over the given spare targets and journal maximum.
func buildEnvWith(t *testing.T, targets segment.Targets, max int64) *env {
	t.Helper()

	dir := t.TempDir()
	seam, err := journaltest.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	e := &env{t: t, dir: dir, seam: seam, counters: &index.Counters{}}

	set := segment.NewSet(harnessCap, nil)
	maker := segment.NewMaker(seam.FS(), set, max, 0, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	e.cache = cache
	e.pool = segment.NewPool(maker, targets, func() error { return nil }, nil)

	// Cleanups run last in, first out: the streams close before the pool's
	// worker stops, and the cache's Shutdown runs last of all.
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	t.Cleanup(func() { e.pool.Stop() })
	e.pool.Start()
	awaitSpares(t, e.pool, targets)

	// The first Durability owns the streams' sink: sinks are made before the
	// streams, so the layer exists first and is started once they do.
	e.d = e.newDurabilityAt(e.dir)
	e.streams = segment.NewStreams(1, harnessIdentity, set, e.pool, cache, e.d.Sink)
	t.Cleanup(func() { _ = e.streams.Close() })
	t.Cleanup(e.d.Stop)
	e.d.Start(e.streams)
	return e
}

// shutdown stops the pool, closes the streams and shuts the cache down, so a
// crash that follows finds no descriptor of the journal's open. Each step is
// repeatable, so the cleanups that run after it find nothing left to do.
func (e *env) shutdown() {
	e.t.Helper()

	e.pool.Stop()
	if err := e.streams.Close(); err != nil {
		e.t.Fatalf("the streams' Close: %v", err)
	}
	if err := e.cache.Shutdown(); err != nil {
		e.t.Fatalf("the cache's Shutdown: %v", err)
	}
}

// newDurability builds a Durability over the env's directory that appends
// through the env's streams, as a reopen in the same process would. The
// streams' sink stays the first layer's, so the new one only loses.
func (e *env) newDurability() *durability.Durability {
	e.t.Helper()
	d := e.newDurabilityAt(e.dir)
	d.BindStreams(e.streams)
	return d
}

// noDomains is a file append domain table that never blocks: the loss tests
// take no domain.
type noDomains struct{}

func (noDomains) Lock(core.FileID) func() { return func() {} }

// newDurabilityAt is newDurability under another spelling of the directory.
func (e *env) newDurabilityAt(dir string) *durability.Durability {
	e.t.Helper()

	var account func(index.HeldDelta)
	if !e.noAcct {
		account = func(d index.HeldDelta) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.accts = append(e.accts, d)
		}
	}
	d, err := durability.New(durability.Config{
		Clock:    journaltest.NewStopwatchClock(),
		Domains:  noDomains{},
		Dir:      dir,
		Counters: e.counters,
		Log:      e.log,
		MarkLoss: func(f core.FileID, x core.Extent, v core.Version) (index.HeldDelta, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.marks = append(e.marks, marked{f, x, v})
			return e.delta, e.markErr
		},
		Account: account,
	})
	if err != nil {
		e.t.Fatalf("durability.New: %v", err)
	}
	return d
}

// accounted returns the deltas Account has been handed so far.
func (e *env) accounted() []index.HeldDelta {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]index.HeldDelta(nil), e.accts...)
}

// marked returns the MarkLoss calls so far.
func (e *env) marked() []marked {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]marked(nil), e.marks...)
}

// awaitSpares waits for the pool's worker to ready every spare the targets
// name.
func awaitSpares(t *testing.T, p *segment.Pool, targets segment.Targets) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		c := p.Counters()
		if c.Spares >= int64(targets.Content) && c.HeadroomBytes >= int64(targets.Headroom)*harnessCap {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied its spares: %+v", c)
		}
		time.Sleep(time.Millisecond)
	}
}

// durableRecordsAndBodies decodes every record the directory's segments would
// hold after a crash now, in segment then file order, with each record's body,
// failing the test on any record that does not verify.
func (e *env) durableRecordsAndBodies() ([]format.RecordHeader, []format.RecordBody) {
	e.t.Helper()

	var hs []format.RecordHeader
	var bs []format.RecordBody
	for id := core.SegmentID(1); id < 32; id++ {
		raw, err := e.seam.Durable(format.SegmentName(id))
		if err != nil {
			continue
		}
		off := int64(format.SegmentHeaderSize)
		for off+format.RecordHeaderSize <= int64(len(raw)) {
			h, err := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], harnessIdentity, id, off)
			if err != nil {
				break // the zero tail of a segment ends the scan
			}
			bodyEnd := off + format.RecordHeaderSize + h.BodySize()
			if bodyEnd > int64(len(raw)) {
				e.t.Fatalf("record at %d of segment %d runs past the durable bytes", off, id)
			}
			body, err := format.VerifyRecordBody(raw[off+format.RecordHeaderSize:bodyEnd], harnessIdentity, h)
			if err != nil {
				e.t.Fatalf("record at %d of segment %d: %v", off, id, err)
			}
			hs = append(hs, h)
			bs = append(bs, body)
			off += h.Size()
		}
	}
	return hs, bs
}

// lossRecords returns the durable loss records' headers and bodies.
func (e *env) lossRecords() ([]format.RecordHeader, []format.Loss) {
	hs, bs := e.durableRecordsAndBodies()
	var lh []format.RecordHeader
	var lb []format.Loss
	for i, h := range hs {
		if h.Kind == format.KindLoss {
			lh = append(lh, h)
			lb = append(lb, bs[i].Loss)
		}
	}
	return lh, lb
}

// file returns a FileID of a test file.
func file(n byte) core.FileID {
	var id [16]byte
	id[0] = n
	return core.NewFileID(id)
}

// loss returns a corrupt, dirty, unsynced loss of one 4 KiB extent of file 1
// whose dropped record had sequence number seq.
func loss(seq core.Seq) durability.Loss {
	return durability.Loss{
		Share:   7,
		File:    file(1),
		Extent:  core.Extent{Off: 4096, Len: 4096},
		Version: core.Version{Counter: uint64(seq) + 100},
		Seq:     seq,
		Reason:  core.LossCorrupt,
	}
}
