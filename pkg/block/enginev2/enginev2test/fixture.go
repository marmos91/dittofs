package enginev2test

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/enginev2"
	"github.com/marmos91/dittofs/pkg/block/enginev2/journalwrap"
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
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
	E     *enginev2.Engine
	FS    *kvstoretest.FS // the store, its share and the share's root
	Opens *openstate.Table
	Seam  *journalv2test.Seam
	Clock *sim.TestClock
	Share enginev2.Share // the share as the engine binds it
}

// New creates the journal, builds the store, the open-state table and the
// engine, and starts the engine with the share bound. Everything is closed
// when the test ends.
func New(t *testing.T) *Fixture {
	t.Helper()

	// Registered first, so its checks run after every other cleanup has
	// closed what it opened.
	prof.Watch(t, prof.Options{})

	ctx := t.Context()
	dir := t.TempDir()
	seam, err := journalv2test.NewSeam(dir)
	if err != nil {
		t.Fatalf("fixture: seam: %v", err)
	}
	clock := sim.NewTestClock(fixtureStart)
	f := &Fixture{Seam: seam, Clock: clock}

	if _, err := journalv2.Init(ctx, dir, fixtureInstall, journalv2.WithClock(clock), journalv2.WithStorage(seam.FS())); err != nil {
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

	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	f.Opens, err = openstate.New(openstate.Deps{Records: f.FS.Store, Clock: clock, Rand: rand.Reader, Logger: logger})
	if err != nil {
		t.Fatalf("fixture: open state: %v", err)
	}
	t.Cleanup(f.Opens.Stop)
	if err := f.Opens.Start(ctx, quietSink{}, noOrder); err != nil {
		t.Fatalf("fixture: open state start: %v", err)
	}
	if err := f.Opens.Recovery().ServeShard(ctx, f.Share.ID, f.Share.Shard, 1); err != nil {
		t.Fatalf("fixture: serve shard: %v", err)
	}

	f.E, err = enginev2.New(enginev2.Deps{
		Devices: []string{dir},
		Open: func(ctx context.Context, dir string, floor journalv2.Version, expect journalv2.JournalID) (enginev2.Journal, error) {
			j, err := journalv2.Open(ctx, dir, floor, expect, journalv2.WithClock(clock), journalv2.WithStorage(seam.FS()))
			if err != nil {
				return nil, err
			}
			return journalwrap.Wrap(j), nil
		},
		Existence: f.FS.Store.Existence(),
		Content:   f.FS.Store.Content(),
		Lifecycle: unbuiltLifecycle{},
		Opens:     f.Opens.Opens(),
		Closed: func(id entity.FileID, r openstate.CloseResult) {
			t.Errorf("fixture: the engine left file %v an orphan: %+v", id, r)
		},
		Clock:  clock,
		Logger: logger,
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		},
	})
	if err != nil {
		t.Fatalf("fixture: engine: %v", err)
	}
	if err := f.E.Apply(Settings); err != nil {
		t.Fatalf("fixture: engine settings: %v", err)
	}
	if err := f.E.Start(ctx, []enginev2.Share{f.Share}); err != nil {
		t.Fatalf("fixture: engine start: %v", err)
	}
	t.Cleanup(func() {
		if err := f.E.Close(context.Background()); err != nil {
			t.Errorf("fixture: engine close: %v", err)
		}
	})
	return f
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
