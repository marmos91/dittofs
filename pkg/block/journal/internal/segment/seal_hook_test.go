package segment

import (
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestAppendParkedAtTheLoopTopRefusesTheSeal is the deterministic guard for
// the seal check at Append's loop top: it stages the exact state an append
// parked on the first take's gate is in when a seal takes the segment while
// it parks — the append has entered the loop, the lock is dropped, and the
// seal is past its drain with its marker read. The park hook holds the
// append at the loop top while the test drives the seal through its own
// first sync, so the append's resume against the seal's closed segment is a
// certainty rather than the scheduler's guess. It catches a check that runs
// only at the entry, before the loop, which no fresh call can fail.
func TestAppendParkedAtTheLoopTopRefusesTheSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := NewSet(64<<10, nil)
	maker := NewMaker(seam.FS(), set, 1<<62, 0, nil)
	cache := NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(8))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	s := NewStream(1, probeIdentity, set, maker, cache)
	defer func() { _ = s.Close() }()

	// Park the appended stream's take: the first Append runs the take,
	// and while it is in flight a second Append parks on the gate — its
	// resume is the moment under test, so the hook stages it directly.
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	appendParkHook = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
		appendParkHook = nil // one park: every later append runs free
	}
	defer func() { appendParkHook = nil }()

	// The first Append takes the segment and lands exactly once at the
	// loop top, where the hook parks it: entered, unlocked, resumable
	// against whatever takes the lock while it waits.
	first := make(chan error, 1)
	go func() {
		payload := make([]byte, 64)
		h, body := writeHeaderOf(probeFileID, payload)
		_, err := s.Append(h, body)
		first <- err
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the first append never reached the loop top's park")
	}

	// The parked append holds the lock nowhere — with the segment still
	// taken (the take has not run at all: the append parks before it),
	// the seal cannot take it, so drive it as the seal would: state
	// sealing is what a seal past its drain leaves the stream in, and
	// the marker is read at the stream's next, which the parked append
	// must not move.
	s.appendMu.Lock()
	s.sealState = sealSealing
	s.appendMu.Unlock()

	// The park lifts: the append resumes at the loop top, where the seal
	// check must refuse it, for the seal holds the segment and its
	// marker covers the whole free span the append would reserve in.
	close(release)
	if err := <-first; !errors.Is(err, ErrSegmentFull) {
		t.Fatalf("the append resumed at the loop top against a sealing stream = %v, want the ErrSegmentFull roll signal", err)
	}

	// The refusal reserved nothing: the position never moved and the take
	// never ran, so the stream still holds its one segment untaken.
	s.appendMu.Lock()
	id := s.id
	s.appendMu.Unlock()
	if id != 0 {
		t.Fatalf("the refused append took the segment (id %d) it must have left untaken", id)
	}
}

// writeHeaderOf builds one write record of the payload given, under the
// probe identity's journal shape.
func writeHeaderOf(fid core.FileID, payload []byte) (format.RecordHeader, format.RecordBody) {
	return format.RecordHeader{
		Kind:    format.KindWrite,
		File:    fid,
		Extent:  core.Extent{Off: 0, Len: int64(len(payload))},
		Version: core.Version{Counter: 1},
		Tag:     7,
	}, format.RecordBody{Payload: payload}
}

// probeFileID is one file the probe identity's records name: a real one, so
// a record that does escape past the refusal encodes instead of panicking —
// the test then fails on its own assertion rather than a broken record.
var probeFileID = core.NewFileID([16]byte{1, 0xaa})
