package enginev2test

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/enginev2"
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/sim"
)

// Settings are the engine settings the fixture applies.
var Settings = enginev2.Settings{
	ExistenceAge:        time.Second,
	GroupCommitMaxFiles: 64,
	CloneMaxLen:         1 << 30,
}

// fixtureStart is when the fixture's clock starts.
var fixtureStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fixtureInstall is the installation ID the fixture's journal is created
// under.
var fixtureInstall = journalv2.InstallationID{'e', 'n', 'g', 'i', 'n', 'e'}

// Fixture is the engine as production composes it: one device's real journal
// on a temporary directory, reached through the journal's crash-simulating
// seam; the real metadata store on an in-memory kv.KV holding one share; the
// real open-state table over the same store, serving the share's shard; and
// one test clock read by all of them. Everything runs under the journal's
// profiler, which fails the test on a leaked goroutine, descriptor or heap.
//
// Every failure of the fixture's own is reported with the prefix "fixture:",
// so a test failing in it is never mistaken for one failing its own check.
type Fixture struct {
	E     *enginev2.Engine // the engine running now; Restart and Crash replace it
	FS    *kvstoretest.FS  // the store, its share and the share's root
	Opens *openstate.Table // the open-state table running now, replaced with E
	Seam  *journalv2test.Seam
	Clock *sim.TestClock
	Share enginev2.Share // the share as the engine binds it

	t      *testing.T
	dir    string
	logger *slog.Logger
	starts uint64 // the processes started, each serving the shard under a new grace instance
}

// New creates the journal and the store, then starts a process over them: the
// open-state table and the engine, the engine started with the share bound.
// Everything is closed when the test ends.
func New(t *testing.T) *Fixture {
	t.Helper()

	// Registered first, so its checks run after every other cleanup has
	// closed what it opened.
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	seam, err := journalv2test.NewSeam(dir)
	if err != nil {
		t.Fatalf("fixture: seam: %v", err)
	}
	clock := sim.NewTestClock(fixtureStart)
	f := &Fixture{Seam: seam, Clock: clock, t: t, dir: dir, logger: slog.New(slog.NewTextHandler(t.Output(), nil))}

	if _, err := journalv2.Init(t.Context(), dir, fixtureInstall, journalv2.WithClock(clock), journalv2.WithStorage(seam.FS())); err != nil {
		t.Fatalf("fixture: journal init: %v", err)
	}

	f.FS = kvstoretest.NewFS(t, memKV)
	f.Share = enginev2.Share{
		ID:         f.FS.Share.ID,
		Tag:        journalv2.ShareTag(f.FS.Share.Tag),
		Shard:      0, // every share has the one shard, zero
		SyncPolicy: journalv2.Bounded,
		State:      enginev2.Serving,
	}

	t.Cleanup(func() { f.stop(true) })
	f.start()
	return f
}

// Restart stops the process cleanly and starts a new one over the same
// journal directory and store.
func (f *Fixture) Restart() {
	f.t.Helper()
	f.stop(true)
	f.start()
}

// Crash simulates a power loss under the journal and starts a new process on
// what it left; the store keeps what it committed. The engine from before the
// crash is closed through a view of the seam the crash ended, so nothing it
// does while closing reaches the directory. opts are the seam's crash
// variants.
func (f *Fixture) Crash(opts ...journalv2test.CrashOption) {
	f.t.Helper()
	if err := f.Seam.Crash(opts...); err != nil {
		f.t.Fatalf("fixture: crash: %v", err)
	}
	f.stop(false)
	f.start()
}

// File creates a regular file in the share's root and returns its handle and
// ID.
func (f *Fixture) File(name string) (entity.Handle, entity.FileID) {
	f.t.Helper()
	h, file := f.FS.Create(f.t, f.FS.Admin, f.FS.Root, name, kvstore.Regular(0o644))
	return h, file.ID
}

// Committed is file h's record as the store has committed it.
func (f *Fixture) Committed(h entity.Handle) entity.File {
	f.t.Helper()
	file, err := f.FS.Store.Files().Get(context.Background(), f.FS.Admin, h)
	if err != nil {
		f.t.Fatalf("fixture: read a file's record: %v", err)
	}
	return file
}

// start builds the open-state table and the engine over the fixture's
// journal directory and store, and starts both.
func (f *Fixture) start() {
	t := f.t
	t.Helper()
	ctx := t.Context()

	var err error
	f.Opens, err = openstate.New(openstate.Deps{Records: f.FS.Store, Clock: f.Clock, Rand: rand.Reader, Logger: f.logger})
	if err != nil {
		t.Fatalf("fixture: open state: %v", err)
	}
	if err := f.Opens.Start(ctx, quietSink{}, noOrder); err != nil {
		t.Fatalf("fixture: open state start: %v", err)
	}
	f.starts++
	if err := f.Opens.Recovery().ServeShard(ctx, f.Share.ID, f.Share.Shard, f.starts); err != nil {
		t.Fatalf("fixture: serve shard: %v", err)
	}

	e, err := enginev2.New(enginev2.Deps{
		Devices: []string{f.dir},
		Open: func(ctx context.Context, dir string, floor journalv2.Version, expect journalv2.JournalID) (enginev2.Journal, error) {
			j, err := journalv2.Open(ctx, dir, floor, expect, journalv2.WithClock(f.Clock), journalv2.WithStorage(f.Seam.FS()))
			if err != nil {
				return nil, err
			}
			return wrapJournal(j), nil
		},
		Existence: f.FS.Store.Existence(),
		Content:   f.FS.Store.Content(),
		Lifecycle: unbuiltLifecycle{},
		Opens:     f.Opens.Opens(),
		Closed: func(id entity.FileID, r openstate.CloseResult) {
			t.Errorf("fixture: the engine left file %v an orphan: %+v", id, r)
		},
		Clock:  f.Clock,
		Logger: f.logger,
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		},
	})
	if err != nil {
		t.Fatalf("fixture: engine: %v", err)
	}
	if err := e.Apply(Settings); err != nil {
		t.Fatalf("fixture: engine settings: %v", err)
	}
	if err := e.Start(ctx, []enginev2.Share{f.Share}); err != nil {
		t.Fatalf("fixture: engine start: %v", err)
	}
	f.E = e
}

// stop closes the engine running now, if one started, and stops the
// open-state table. clean is false after a crash, when the engine's close is
// expected to fail and is only logged.
func (f *Fixture) stop(clean bool) {
	f.t.Helper()
	if f.E != nil {
		if err := f.E.Close(context.Background()); err != nil {
			if clean {
				f.t.Errorf("fixture: engine close: %v", err)
			} else {
				f.t.Logf("fixture: closing the crashed engine: %v", err)
			}
		}
		f.E = nil
	}
	if f.Opens != nil {
		f.Opens.Stop()
		f.Opens = nil
	}
}

// memKV is a fresh in-memory kv.KV, closed when the test ends.
func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatalf("fixture: store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}

// errNoLifecycle answers every call of the fixture's Lifecycle.
var errNoLifecycle = errors.New("fixture: the store's Lifecycle view is not built")

// unbuiltLifecycle stands where the store's Lifecycle view goes, which the
// store does not have yet: every call is refused.
type unbuiltLifecycle struct{}

func (unbuiltLifecycle) JournalID(context.Context, string) ([16]byte, error) {
	return [16]byte{}, errNoLifecycle
}

func (unbuiltLifecycle) VersionLimits(context.Context, []entity.ShareID) (map[entity.ShareID]entity.JournalVersion, error) {
	return nil, errNoLifecycle
}

func (unbuiltLifecycle) RaiseVersionLimits(context.Context, map[entity.ShareID]entity.JournalVersion) error {
	return errNoLifecycle
}

func (unbuiltLifecycle) RaiseIncarnations(context.Context, []entity.ShardID) error {
	return errNoLifecycle
}

// quietSink is open state's Sink with no client and no service behind it:
// the fixture serves no protocol, so nothing it raises has anyone to reach.
type quietSink struct{}

func (quietSink) ClientEnded(entity.ClientID, []openstate.ShardID, openstate.RevokeReason) {}
func (quietSink) Closed(entity.FileID, openstate.CloseResult)                              {}
func (quietSink) LockWaitOver(openstate.LockWaiter, bool)                                  {}
func (quietSink) RecallGrant(openstate.CachingGrant)                                       {}
func (quietSink) RecallAny(entity.ClientID, int)                                           {}
func (quietSink) Revoked(openstate.Revocation)                                             {}
func (quietSink) Notify(entity.ClientID, openstate.WatchID, []openstate.Change)            {}
func (quietSink) GraceEnded(entity.ShareID, openstate.ShardID)                             {}

// noOrder takes no admission lock: the fixture has no service holding one.
func noOrder(entity.FileID) func() { return func() {} }
