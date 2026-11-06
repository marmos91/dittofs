package engine_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// startDevice is the one journal device of a start test.
const startDevice = "dev"

// starter is the engine composed for a start test, not yet started: over a
// fake journal that logs every call the engine makes on it, the real store on
// an in-memory kv.KV holding two shares, and a Deps.Open the test answers.
type starter struct {
	e    *engine.Engine
	fs   *kvstoretest.FS
	j    *logJournal
	lc   *startLifecycle
	logs logRecorder
	one  engine.Share // the store's first share
	two  engine.Share // a second share of the store
	id   journal.JournalID

	mu      sync.Mutex
	opens   []openCall
	openErr error // what Open answers in place of the journal
	gate    *gate // holds the next Open at it, once
}

// openCall is one call of Deps.Open.
type openCall struct {
	dir    string
	floor  journal.Version
	expect journal.JournalID
}

// starterConfig is what a test asks of newStarter.
type starterConfig struct {
	noRecord bool
	deps     func(*engine.Deps)
}

// starterOption is one thing a test asks of newStarter.
type starterOption func(*starterConfig)

// noRecord leaves the device with no journal recorded in the store.
func noRecord() starterOption {
	return func(c *starterConfig) {
		c.noRecord = true
	}
}

// withStartDeps changes the engine's Deps before New.
func withStartDeps(f func(*engine.Deps)) starterOption {
	return func(c *starterConfig) {
		c.deps = f
	}
}

func newStarter(t *testing.T, opts ...starterOption) *starter {
	t.Helper()
	prof.Watch(t, prof.Options{})

	var c starterConfig

	for _, o := range opts {
		o(&c)
	}

	s := &starter{
		j: &logJournal{
			FakeJournal: enginetest.NewFakeJournal(),
		},
		logs: newLogRecorder(),
		id:   journal.JournalID{'A'},
	}
	s.fs = kvstoretest.NewFS(t, memKV)
	s.lc = &startLifecycle{
		Lifecycle: s.fs.Store.Lifecycle(),
	}
	s.one = enginetest.ShareOf(t, s.fs.Store, s.fs.Share.ID, 1<<30)
	s.two = enginetest.AddShare(t, s.fs.Store, "two", 1<<30)

	if !c.noRecord {
		err := s.fs.Store.RecordJournal(t.Context(), s.id, entity.NodeID{1}, entity.JournalDir(startDevice))
		if err != nil {
			t.Fatal(err)
		}
	}

	d := engine.Deps{
		Node:      entity.NodeID{1},
		Devices:   []string{startDevice},
		Open:      s.open,
		Existence: s.fs.Store.Existence(),
		Content:   s.fs.Store.Content(),
		Lifecycle: s.lc,
		Holds:     noHolds{},
		Clock:     sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Logger:    slog.New(s.logs),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {
			}
		},
	}

	if c.deps != nil {
		c.deps(&d)
	}

	e, err := engine.New(d)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(e.CloseGroupCommits)

	if err := e.Apply(enginetest.Settings); err != nil {
		t.Fatal(err)
	}

	s.e = e

	return s
}

// open is the starter's Deps.Open: it records the call, holds it at the gate
// when one is set, and answers the fake journal or openErr.
func (s *starter) open(_ context.Context, dir string, floor journal.Version, expect journal.JournalID) (engine.Journal, error) {
	s.mu.Lock()
	s.opens = append(s.opens, openCall{dir, floor, expect})
	g, err := s.gate, s.openErr
	s.gate = nil
	s.mu.Unlock()

	if g != nil {
		close(g.entered)
		<-g.release
	}

	if err != nil {
		return nil, err
	}

	if err := s.j.add("open"); err != nil {
		return nil, err
	}

	return s.j, nil
}

// openCalls is every Open call so far.
func (s *starter) openCalls() []openCall {
	s.mu.Lock()

	defer s.mu.Unlock()

	return slices.Clone(s.opens)
}

// start starts the engine with the starter's two shares.
func (s *starter) start(t *testing.T) error {
	t.Helper()

	return s.e.Start(t.Context(), []engine.Share{s.one, s.two})
}

// write writes one byte to a new file of share id.
func (s *starter) write(t *testing.T, id entity.ShareID) error {
	t.Helper()

	opts := engine.WriteOpts{
		Done: func() {
		},
	}
	_, err := s.e.Write(t.Context(), id, entity.NewFileID(), 0, []byte("x"), opts)

	return err
}

// seed leaves tag holding one byte in the fake journal, with no handle of it
// open, as a share the journal served before the restart does.
func (s *starter) seed(t *testing.T, tag journal.ShareTag) {
	t.Helper()

	j := s.j.FakeJournal

	if err := j.SetVersionLimit(journal.Version{Counter: 1 << 40}); err != nil {
		t.Fatal(err)
	}

	h, err := j.Share(tag, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}

	f := journal.NewFileID(entity.NewFileID())

	if _, err := h.WriteAt(t.Context(), f, 0, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// startLifecycle is the store's lifecycle view with each call named in fail
// failing with its error, and the share records in removed added to what
// Tags answers, as the removal's steps would record them.
type startLifecycle struct {
	engine.Lifecycle
	fail    map[string]error
	removed map[entity.ShareID]entity.TagRecord
}

func (l *startLifecycle) JournalID(
	ctx context.Context,
	node entity.NodeID,
	device entity.JournalDir,
) ([16]byte, error) {
	if err := l.fail["JournalID"]; err != nil {
		return [16]byte{}, err
	}

	return l.Lifecycle.JournalID(ctx, node, device)
}

func (l *startLifecycle) Tags(ctx context.Context) (map[entity.ShareID]entity.TagRecord, error) {
	if err := l.fail["Tags"]; err != nil {
		return nil, err
	}

	tags, err := l.Lifecycle.Tags(ctx)
	if err != nil {
		return nil, err
	}

	for id, r := range l.removed {
		tags[id] = r
	}

	return tags, nil
}

func (l *startLifecycle) VersionLimits(
	ctx context.Context,
	shares []entity.ShareID,
) (map[entity.ShareID]entity.JournalVersion, error) {
	if err := l.fail["VersionLimits"]; err != nil {
		return nil, err
	}

	return l.Lifecycle.VersionLimits(ctx, shares)
}

func (l *startLifecycle) RaiseVersionLimits(ctx context.Context, to map[entity.ShareID]entity.JournalVersion) error {
	if err := l.fail["RaiseVersionLimits"]; err != nil {
		return err
	}

	return l.Lifecycle.RaiseVersionLimits(ctx, to)
}

// logJournal is the engine's view of a fake journal: it logs each call the
// engine makes on it, and on the handles it returns, in order; fails a call
// named in fail with its error; and holds the next SetVersionLimit at the
// gate armed, once.
type logJournal struct {
	*enginetest.FakeJournal
	fail      map[string]error
	limitGate atomic.Pointer[gate]

	mu     sync.Mutex
	calls  []string
	marks  []journal.Version
	shared []sharedCall
}

// sharedCall is one Share the engine made.
type sharedCall struct {
	tag   journal.ShareTag
	limit int64
	floor journal.Version
}

// add logs call and answers the error fail names for it.
func (j *logJournal) add(call string) error {
	j.mu.Lock()

	defer j.mu.Unlock()

	j.calls = append(j.calls, call)

	return j.fail[call]
}

// log is every call logged so far.
func (j *logJournal) log() []string {
	j.mu.Lock()

	defer j.mu.Unlock()

	return slices.Clone(j.calls)
}

// called reports whether call was logged.
func (j *logJournal) called(call string) bool {
	return slices.Contains(j.log(), call)
}

func (j *logJournal) Share(
	tag journal.ShareTag,
	limit int64,
	floor journal.Version,
) (engine.Handle, error) {
	if err := j.add(fmt.Sprintf("share %d", tag)); err != nil {
		return nil, err
	}

	h, err := j.FakeJournal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}

	j.mu.Lock()
	j.shared = append(j.shared, sharedCall{tag, limit, floor})
	j.mu.Unlock()

	lh := &logHandle{
		Handle: h,
		j:      j,
		tag:    tag,
	}

	return lh, nil
}

func (j *logJournal) Forget(ctx context.Context, tag journal.ShareTag) error {
	if err := j.add(fmt.Sprintf("forget %d", tag)); err != nil {
		return err
	}

	return j.FakeJournal.Forget(ctx, tag)
}

func (j *logJournal) SetVersionLimit(mark journal.Version) error {
	if err := j.add("limit"); err != nil {
		return err
	}

	j.mu.Lock()
	j.marks = append(j.marks, mark)
	j.mu.Unlock()

	if g := j.limitGate.Swap(nil); g != nil {
		close(g.entered)
		<-g.release
	}

	return j.FakeJournal.SetVersionLimit(mark)
}

func (j *logJournal) Draw() (journal.Version, error) {
	if err := j.add("draw"); err != nil {
		return journal.Version{}, err
	}

	return j.FakeJournal.Draw()
}

func (j *logJournal) Close() error {
	if err := j.add("close journal"); err != nil {
		return err
	}

	return j.FakeJournal.Close()
}

// mark is the last mark SetVersionLimit was given.
func (j *logJournal) mark() journal.Version {
	j.mu.Lock()

	defer j.mu.Unlock()

	return j.marks[len(j.marks)-1]
}

// shareCalls is every Share the engine made.
func (j *logJournal) shareCalls() []sharedCall {
	j.mu.Lock()

	defer j.mu.Unlock()

	return slices.Clone(j.shared)
}

// logHandle logs its writes and its Close.
type logHandle struct {
	engine.Handle
	j   *logJournal
	tag journal.ShareTag
}

func (h *logHandle) WriteAt(
	ctx context.Context,
	id journal.FileID,
	off int64,
	p []byte,
	mtime int64,
) (journal.Version, error) {
	if err := h.j.add(fmt.Sprintf("write %d", h.tag)); err != nil {
		return journal.Version{}, err
	}

	return h.Handle.WriteAt(ctx, id, off, p, mtime)
}

func (h *logHandle) Close() error {
	if err := h.j.add(fmt.Sprintf("close handle %d", h.tag)); err != nil {
		return err
	}

	return h.Handle.Close()
}

// journalLost reports whether st raises journal_lost for the start device.
func journalLost(st engine.Stats) bool {
	for _, c := range st.Journals[startDevice].Conditions {
		if c.Name == "journal_lost" {
			return true
		}
	}

	return false
}

// Start opens the device's journal with the identity the store records
// for it and the floor the store's versions give: the larger of a share's
// version floor and its version limit. This checks only what Open is handed;
// that the journal then issues above it is the conformance row on a device
// loss, over the real journal.
func TestStartOpensWithTheRecordedIdentityAndFloor(t *testing.T) {
	for _, c := range []struct {
		name         string
		floor, limit uint64 // offsets above the store's floor
	}{
		{"the limit is larger", 1e12, 2e12},
		{"the floor is larger", 2e12, 1e12},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStarter(t)
			ctx := t.Context()
			_, file := s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o644))

			base, err := s.fs.Store.Content().VersionFloor(ctx, []entity.ShareID{s.one.ID})
			if err != nil {
				t.Fatal(err)
			}

			ex := entity.VersionOf(base.Counter() + c.floor)
			lim := entity.VersionOf(base.Counter() + c.limit)
			commitShareAt(t, s.fs, s.one.ID, file.ID, ex)

			to := map[entity.ShareID]entity.JournalVersion{
				s.one.ID: lim,
			}

			if err := s.fs.Store.Lifecycle().RaiseVersionLimits(ctx, to); err != nil {
				t.Fatal(err)
			}

			floor, err := s.fs.Store.Content().VersionFloor(ctx, []entity.ShareID{s.one.ID})
			if err != nil {
				t.Fatal(err)
			}

			want := entity.MaxVersion(floor, lim)

			if err := s.start(t); err != nil {
				t.Fatalf("Start: %v", err)
			}

			opens := s.openCalls()
			if len(opens) != 1 {
				t.Fatalf("Open was called %d times, want once", len(opens))
			}

			if opens[0].dir != startDevice || opens[0].expect != s.id {
				t.Errorf("Open(%q, expect %x), want (%q, the recorded %x)", opens[0].dir, opens[0].expect, startDevice, s.id)
			}

			if opens[0].floor.Counter != want.Counter() {
				t.Errorf("Open got floor %d, want the larger of floor and limit, %d", opens[0].floor.Counter, want.Counter())
			}
		})
	}
}

// commitShareAt commits existence for file of share at v, so the share's
// version floor is at least v.
func commitShareAt(t *testing.T, fs *kvstoretest.FS, share entity.ShareID, file entity.FileID, v entity.JournalVersion) {
	t.Helper()

	now := time.Now()
	pending := []kvstore.PendingExistence{
		{
			Share: share,
			File:  file,
			Ops: []kvstore.PendingOp{
				{
					Version: v,
					Kind:    kvstore.OpWrite,
					Extent:  entity.Extent{Length: 4096},
					Mtime:   now,
					Ctime:   now,
				},
			},
		},
	}

	failed, err := fs.Store.Existence().Commit(t.Context(), pending, nil)
	if err != nil || failed[file] != nil {
		t.Fatalf("commit existence at %v: %v, %v", v, err, failed[file])
	}
}

// A device whose recorded journal is missing, foreign or unidentifiable
// has every share on it refused by name, with its device, a share the store
// records and Start was not handed included, and journal_lost raised, and
// Start succeeds. The directory is opened once and nothing else is done on
// it. A refused share answers ErrDelay with DelayFrozen, never ErrStale, and
// its refusal is logged at Warn with the share and the device; a share being
// removed is not named refused and still answers ErrStale.
func TestStartRefusesALostJournalByName(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"missing", journal.ErrNoJournal},
		{"foreign", &journal.IdentityError{Found: journal.JournalID{'B'}}},
		{"unidentified", journal.ErrUnidentified},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStarter(t)
			s.openErr = c.err
			unhanded := enginetest.AddShare(t, s.fs.Store, "three", 1<<30)
			forgotten := entity.NewShareID()
			offloaded := entity.NewShareID()
			s.lc.removed = map[entity.ShareID]entity.TagRecord{
				forgotten: {
					Tag:   91,
					State: entity.TagForgotten,
				},
				offloaded: {
					Tag:   92,
					State: entity.TagOffloaded,
				},
			}

			if err := s.start(t); err != nil {
				t.Fatalf("Start with a lost journal = %v, want nil", err)
			}

			st := s.e.Stats()

			for _, sh := range []engine.Share{s.one, s.two, unhanded} {
				got := st.Shares[sh.ID]

				if got.Device != startDevice || !errors.Is(got.Refused, engine.ErrJournalLost) || !errors.Is(got.Refused, c.err) {
					t.Errorf("share %v: device %q, refused %v; want %q, the lost journal's %v", sh.ID, got.Device, got.Refused, startDevice, c.err)
				}

				if !warned(s.logs, sh.ID, startDevice) {
					t.Errorf("share %v: no Warn naming it and its device", sh.ID)
				}
			}

			for _, id := range []entity.ShareID{forgotten, offloaded} {
				if got, ok := st.Shares[id]; ok {
					t.Errorf("removed share %v is named refused: %+v", id, got)
				}
			}

			if !journalLost(st) {
				t.Errorf("journal conditions %+v, want journal_lost", st.Journals[startDevice])
			}

			want := engine.Subject{
				Scope: engine.ScopeJournal,
				ID:    hex.EncodeToString(s.id[:]),
				Name:  startDevice,
			}

			for _, cond := range st.Journals[startDevice].Conditions {
				if cond.Subject != want {
					t.Errorf("condition %s is about %+v, want the recorded journal %+v", cond.Name, cond.Subject, want)
				}
			}

			if n := len(s.openCalls()); n != 1 {
				t.Errorf("Open was called %d times, want once", n)
			}

			if log := s.j.log(); len(log) != 0 {
				t.Errorf("calls on the refused journal: %v, want none", log)
			}

			if err := s.write(t, s.one.ID); !isDelay(err, vfserr.DelayFrozen) {
				t.Errorf("a write on the refused share = %v, want ErrDelay(DelayFrozen)", err)
			}

			for _, id := range []entity.ShareID{forgotten, offloaded} {
				if err := s.e.BindingOf(id); !errors.Is(err, vfserr.ErrStale) {
					t.Errorf("removed share %v's binding = %v, want ErrStale", id, err)
				}
			}
		})
	}
}

// What Stats returns is the caller's: changing its maps or its condition
// lists changes nothing the engine reports next.
func TestStatsIsACopy(t *testing.T) {
	s := newStarter(t)
	s.openErr = journal.ErrNoJournal

	if err := s.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mine := s.e.Stats()
	clear(mine.Shares)
	mine.Journals[startDevice].Conditions[0].Name = "changed"

	if st := s.e.Stats(); len(st.Shares) == 0 || !journalLost(st) {
		t.Errorf("after the caller changed its copy, Stats is %+v", st)
	}
}

// warned reports whether a Warn record names share and device.
func warned(logs logRecorder, share entity.ShareID, device string) bool {
	for _, r := range logs.warnings() {
		var gotShare, gotDevice bool

		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "share":
				gotShare = a.Value.String() == share.String()
			case "device":
				gotDevice = a.Value.String() == device
			}

			return true
		})

		if gotShare && gotDevice {
			return true
		}
	}

	return false
}

// With no journal recorded for the device and one in its directory, the
// device's shares are refused by name as not recorded, the refusal names the
// identity and installation the directory holds, journal_lost is not raised,
// and Start succeeds: the journal is foreign, or of this installation and
// not recorded, never a lost device.
func TestStartRefusesAJournalNotRecorded(t *testing.T) {
	s := newStarter(t, noRecord())
	found := &journal.IdentityError{
		Found:        journal.JournalID{'B'},
		Installation: journal.InstallationID{'I'},
		Segments:     true,
	}
	s.openErr = found

	if err := s.start(t); err != nil {
		t.Fatalf("Start with a journal not recorded = %v, want nil", err)
	}

	st := s.e.Stats()
	got := st.Shares[s.one.ID]

	var ie *journal.IdentityError

	if !errors.Is(got.Refused, engine.ErrNotRecorded) || !errors.As(got.Refused, &ie) {
		t.Fatalf("refused %v, want not recorded, naming the identity", got.Refused)
	}

	if ie.Found != found.Found || ie.Installation != found.Installation || got.Device != startDevice {
		t.Errorf("refusal names %x of %x on %q, want %x of %x on %q", ie.Found, ie.Installation, got.Device, found.Found, found.Installation, startDevice)
	}

	if journalLost(st) {
		t.Error("journal_lost raised for a journal not recorded")
	}

	if s.openCalls()[0].expect != (journal.JournalID{}) {
		t.Errorf("Open expected %x, want zero", s.openCalls()[0].expect)
	}
}

// With no journal recorded for the device and none in its directory,
// Start fails as uninitialised, refusing nothing and attaching nothing, and
// leaves the engine as built, so a later Start runs.
func TestStartFailsOnAnUninitialisedDevice(t *testing.T) {
	s := newStarter(t, noRecord())
	s.openErr = journal.ErrNoJournal

	err := s.start(t)
	if !errors.Is(err, journal.ErrNoJournal) {
		t.Fatalf("Start on an uninitialised device = %v, want it to fail with ErrNoJournal", err)
	}

	if st := s.e.Stats(); len(st.Shares) != 0 {
		t.Errorf("shares reported %v, want none", st.Shares)
	}

	if err := s.e.BindingOf(s.one.ID); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("the share's binding after a failed Start: %v, want none", err)
	}

	s.openErr = nil

	err = s.fs.Store.RecordJournal(t.Context(), s.id, entity.NodeID{1}, entity.JournalDir(startDevice))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.start(t); err != nil {
		t.Errorf("Start again after a failed one = %v, want it to run as on a new engine", err)
	}
}

// With no journal recorded for the device and segments with no format
// file in its directory, the device's shares are refused by name and Start
// succeeds: the device holds data no journal identifies, never an
// uninitialised one.
func TestStartRefusesAnUnidentifiedDirectory(t *testing.T) {
	s := newStarter(t, noRecord())
	s.openErr = journal.ErrUnidentified

	if err := s.start(t); err != nil {
		t.Fatalf("Start over segments with no format file = %v, want nil", err)
	}

	st := s.e.Stats()

	if got := st.Shares[s.one.ID]; !errors.Is(got.Refused, engine.ErrUnidentified) || got.Device != startDevice {
		t.Errorf("share refused %v on %q, want unidentified on %q", got.Refused, got.Device, startDevice)
	}

	if journalLost(st) {
		t.Error("journal_lost raised for a journal not recorded")
	}
}

// An error Open meets in a check, rather than a refusal it makes, fails
// Start, which refuses no share for it.
func TestStartFailsOnACheckError(t *testing.T) {
	s := newStarter(t)
	ioErr := &fs.PathError{
		Op:   "statfs",
		Path: startDevice,
		Err:  errors.New("input/output error"),
	}
	s.openErr = ioErr

	if err := s.start(t); !errors.Is(err, ioErr) {
		t.Fatalf("Start on a check's error = %v, want it to fail with that error", err)
	}

	if st := s.e.Stats(); len(st.Shares) != 0 || journalLost(st) {
		t.Errorf("stats %+v, want no refusal and no condition", st)
	}
}

// A step of Start that fails returns its error, closes in reverse every
// handle and the journal it opened, and publishes nothing; a store read that
// fails before Open fails Start before any journal is opened.
func TestAFailedStartClosesInReverse(t *testing.T) {
	const forgottenTag = 9

	for _, c := range []struct {
		name   string
		store  string // the lifecycle call that fails
		floor  bool   // the version floor read fails
		call   string // the journal call that fails, by its log name
		opened bool   // the journal was opened
		closes []string
	}{
		{name: "JournalID", store: "JournalID"},
		{name: "Tags", store: "Tags"},
		{name: "VersionLimits", store: "VersionLimits"},
		{name: "VersionFloor", floor: true},
		{name: "Forget", call: fmt.Sprintf("forget %d", forgottenTag), opened: true, closes: []string{"close journal"}},
		{name: "second Share", call: "share 0", opened: true},
		{name: "RaiseVersionLimits", store: "RaiseVersionLimits", opened: true},
		{name: "SetVersionLimit", call: "limit", opened: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStarter(t, withStartDeps(func(d *engine.Deps) {
				if c.floor {
					d.Content = faultContent{
						Content: d.Content,
						err:     errInjected,
					}
				}
			}))

			if c.store != "" {
				s.lc.fail = map[string]error{c.store: errInjected}
			}

			forgotten := entity.NewShareID()
			s.lc.removed = map[entity.ShareID]entity.TagRecord{
				forgotten: {
					Tag:   forgottenTag,
					State: entity.TagForgotten,
				},
			}
			s.seed(t, forgottenTag)

			one, two := s.one.Tag, s.two.Tag
			closes := c.closes

			if c.call == "share 0" {
				c.call = fmt.Sprintf("share %d", two)
				closes = []string{fmt.Sprintf("close handle %d", one), "close journal"}
			}

			if closes == nil && c.opened {
				closes = []string{fmt.Sprintf("close handle %d", two), fmt.Sprintf("close handle %d", one), "close journal"}
			}

			if c.call != "" {
				s.j.fail = map[string]error{c.call: errInjected}
			}

			err := s.start(t)
			if !errors.Is(err, errInjected) {
				t.Fatalf("Start = %v, want the injected failure", err)
			}

			if !c.opened && len(s.openCalls()) != 0 {
				t.Errorf("Open was called after the store failed")
			}

			var got []string

			for _, call := range s.j.log() {
				if strings.HasPrefix(call, "close") {
					got = append(got, call)
				}
			}

			if !slices.Equal(got, closes) {
				t.Errorf("closes %v, want %v", got, closes)
			}

			for _, id := range []entity.ShareID{s.one.ID, forgotten} {
				if err := s.e.BindingOf(id); !isDelay(err, vfserr.DelayFrozen) {
					t.Errorf("share %v's binding after a failed Start: %v, want none", id, err)
				}
			}
		})
	}
}

// A share whose removal reads forgotten has its tag forgotten before any
// share attaches, wherever the journal still names it; one whose removal
// reads offloaded is not attached and not forgotten; both answer ErrStale.
func TestStartForgetsFinishedRemovalsAndAttachesNoRemovedShare(t *testing.T) {
	s, forgotten, offloaded := startedWithRemovals(t)

	log := s.j.log()
	forgetAt := slices.Index(log, "forget 91")
	firstShare := slices.IndexFunc(log, func(call string) bool {
		return strings.HasPrefix(call, "share ")
	})

	if forgetAt < 0 || firstShare < 0 || forgetAt > firstShare {
		t.Errorf("calls %v, want forget 91 before the first share", log)
	}

	for _, call := range []string{"share 92", "forget 92", "forget 93"} {
		if s.j.called(call) {
			t.Errorf("calls %v include %q", log, call)
		}
	}

	for _, id := range []entity.ShareID{forgotten, offloaded} {
		if err := s.e.BindingOf(id); !errors.Is(err, vfserr.ErrStale) {
			t.Errorf("removed share %v's binding = %v, want ErrStale", id, err)
		}
	}
}

// startedWithRemovals starts an engine whose store records two shares being
// removed, one forgotten with its tag still in the journal and one
// offloaded, and a third forgotten tag the journal no longer names. The
// offloaded share is handed to Start too.
func startedWithRemovals(t *testing.T) (s *starter, forgotten, offloaded entity.ShareID) {
	t.Helper()

	s = newStarter(t)
	forgotten = entity.NewShareID()
	offloaded = entity.NewShareID()
	s.lc.removed = map[entity.ShareID]entity.TagRecord{
		forgotten: {
			Tag:   91,
			State: entity.TagForgotten,
		},
		offloaded: {
			Tag:   92,
			State: entity.TagOffloaded,
		},
		entity.NewShareID(): {
			Tag:   93,
			State: entity.TagForgotten,
		},
	}
	s.seed(t, 91)
	s.seed(t, 92)

	handed := engine.Share{
		ID:    offloaded,
		Tag:   92,
		State: engine.Serving,
	}

	if err := s.e.Start(t.Context(), []engine.Share{s.one, handed}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return s, forgotten, offloaded
}

// A Bind of a share being removed answers ErrStale, opens no handle and
// changes nothing, never a state change that reports it served.
func TestBindOfARemovedShareIsStale(t *testing.T) {
	s, forgotten, offloaded := startedWithRemovals(t)
	table := s.e.TableForTest()
	shares := len(s.j.shareCalls())

	for _, sh := range []engine.Share{
		{ID: forgotten, Tag: 91, State: engine.Serving},
		{ID: offloaded, Tag: 92, State: engine.Serving},
	} {
		if err := s.e.Bind(t.Context(), sh); !errors.Is(err, vfserr.ErrStale) {
			t.Errorf("Bind of removed share %v = %v, want ErrStale", sh.ID, err)
		}
	}

	if n := len(s.j.shareCalls()); n != shares {
		t.Errorf("Bind opened %d handles", n-shares)
	}

	if s.e.TableForTest() != table {
		t.Error("Bind swapped the share table")
	}
}

// While Start runs, Bind, Unbind and Drop answer ErrDelay with
// DelayFrozen, the share not attached yet, and Bind attaches nothing.
func TestLifecycleCallsDuringStartAreDelayed(t *testing.T) {
	s := newStarter(t)
	g := newGate(t)
	s.gate = g
	done := make(chan error, 1)

	go func() {
		done <- s.start(t)
	}()

	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("Start returned %v before it opened the journal", err)
	}

	if err := s.e.Bind(t.Context(), s.one); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Bind during Start = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Unbind(t.Context(), s.one.ID, false); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Unbind during Start = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Drop(t.Context(), s.one.ID); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Drop during Start = %v, want ErrDelay(DelayFrozen)", err)
	}

	if n := len(s.j.shareCalls()); n != 0 {
		t.Errorf("%d Share calls while Open was held", n)
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// A second Start after one that succeeded is refused, and opens nothing.
func TestASecondStartIsRefused(t *testing.T) {
	s := newStarter(t)

	if err := s.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}

	table := s.e.TableForTest()

	if err := s.start(t); !errors.Is(err, engine.ErrStarted) {
		t.Errorf("a second Start = %v, want the already-started refusal", err)
	}

	if n := len(s.openCalls()); n != 1 {
		t.Errorf("Open was called %d times, want once", n)
	}

	if s.e.TableForTest() != table {
		t.Error("the second Start swapped the share table")
	}
}

// A Start while one runs is refused and opens nothing, and the first goes
// on to serve.
func TestAStartWhileOneRunsIsRefused(t *testing.T) {
	s := newStarter(t)
	g := newGate(t)
	s.gate = g
	done := make(chan error, 1)

	go func() {
		done <- s.start(t)
	}()

	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("Start returned %v before it opened the journal", err)
	}

	if err := s.start(t); !errors.Is(err, engine.ErrStarted) {
		t.Errorf("a Start while one runs = %v, want the already-started refusal", err)
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("the first Start: %v", err)
	}

	if n := len(s.openCalls()); n != 1 {
		t.Errorf("Open was called %d times, want once", n)
	}

	if err := s.e.BindingOf(s.one.ID); err != nil {
		t.Errorf("share %v after the first Start: %v, want bound", s.one.ID, err)
	}
}

// With more than one journal device Start fails as not built, as no
// shard record names its share's journal yet, and opens nothing.
func TestStartRefusesMoreThanOneDevice(t *testing.T) {
	s := newStarter(t, withStartDeps(func(d *engine.Deps) {
		d.Devices = []string{startDevice, "other"}
	}))

	if err := s.start(t); !errors.Is(err, engine.ErrUnbuilt) {
		t.Errorf("Start over two devices = %v, want the unbuilt refusal", err)
	}

	if n := len(s.openCalls()); n != 0 {
		t.Errorf("Open was called %d times, want never", n)
	}
}

// No share is served before its journal has its version limit: until
// SetVersionLimit returns a write answers ErrDelay with DelayFrozen and
// reaches no journal, and by then every served share's limit is committed
// in the store, a block above the journal's next version. Each served share
// then has one handle with its own tag and limit, and writes.
func TestNoWriteBeforeTheVersionLimit(t *testing.T) {
	s := newStarter(t)
	s.seed(t, 99) // the journal has a limit that is not its next version
	g := newGate(t)
	s.j.limitGate.Store(g)
	done := make(chan error, 1)

	go func() {
		done <- s.start(t)
	}()

	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("Start returned %v before it set the version limit", err)
	}

	if err := s.write(t, s.one.ID); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("a write before SetVersionLimit = %v, want ErrDelay(DelayFrozen)", err)
	}

	for _, call := range s.j.log() {
		if strings.HasPrefix(call, "write") || call == "draw" {
			t.Errorf("%q before SetVersionLimit", call)
		}
	}

	limits, err := s.fs.Store.Lifecycle().VersionLimits(t.Context(), []entity.ShareID{s.one.ID, s.two.ID})
	if err != nil {
		t.Fatal(err)
	}

	st := s.j.Stats()
	mark := s.j.mark()
	want := st.NextVersion.Counter + 1<<24

	if st.VersionLimit == st.NextVersion {
		t.Fatalf("setup: the journal's limit is its next version, %d", st.NextVersion.Counter)
	}

	if mark.Counter != want {
		t.Errorf("SetVersionLimit got %d, want a block above the next version, %d", mark.Counter, want)
	}

	for id, l := range limits {
		if l.Counter() < mark.Counter {
			t.Errorf("share %v's committed limit %d is below the mark %d", id, l.Counter(), mark.Counter)
		}
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}

	calls := s.j.shareCalls()
	if len(calls) != 2 {
		t.Fatalf("Share calls %v, want one per served share", calls)
	}

	for i, sh := range []engine.Share{s.one, s.two} {
		if c := calls[i]; c.tag != sh.Tag || c.limit != sh.Limit {
			t.Errorf("Share(%d, %d), want the share's (%d, %d)", c.tag, c.limit, sh.Tag, sh.Limit)
		}
	}

	if err := s.write(t, s.one.ID); err != nil {
		t.Errorf("a write after Start: %v", err)
	}
}
