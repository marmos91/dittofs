package fs

// Async, non-blocking migration of a pre-journal LOCAL-ONLY layout into the
// journal. Opening the store archives the legacy dirs aside (readable, not the
// terminal backup used by remote-backed shares) and returns immediately; the
// bytes are re-ingested lazily:
//
//   - a background goroutine (driven by the shares service, which owns the
//     engine needed for the final rollup) replays every archived log into the
//     journal, then deletes the archived dirs;
//   - a read that arrives before its payload has been drained faults that ONE
//     payload in synchronously, so a read never observes zeros.
//
// Both paths funnel through the same per-payload sync.Once, so a read and the
// background drain can race a payload without double-writing. The legacy bytes
// stay on disk (under the archive) until the whole share is drained, so a crash
// mid-migration re-runs cleanly on the next open (idempotent: replaying the same
// records overwrites the same journal intervals).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// legacyMigration holds the state for draining one share's archived pre-journal
// logs into its journal. Nil on FSStore when there is nothing to migrate.
type legacyMigration struct {
	store       *journal.Store
	logsBackup  string            // <dir>/logs.pre-journal-backup
	blobsBackup string            // <dir>/blobs.pre-journal-backup
	payloads    map[string]string // payloadID -> archived log path (immutable after setup)
	onces       sync.Map          // payloadID -> *legacyMigrateOnce
	done        atomic.Bool
}

type legacyMigrateOnce struct {
	once sync.Once
	err  error
}

// dirHasEntries reports whether path is a directory holding at least one entry.
// A missing directory reports false.
func dirHasEntries(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// setupLegacyLocalOnlyMigration inspects dir for a pre-journal local-only
// layout and, when it can be recovered from the append logs alone, archives it
// aside and returns a ready-to-drain migration (with a nil store — the caller
// binds the journal once it is open). It returns (nil, nil) when there is
// nothing to migrate, and a wrapped ErrLegacyLocalFormat when a legacy layout
// exists but is NOT safely recoverable (any compacted/torn log, or blob data
// with no covering logs) — in that case nothing is moved, so the bytes stay on
// disk for manual recovery.
func setupLegacyLocalOnlyMigration(dir string) (*legacyMigration, error) {
	logsBackup := filepath.Join(dir, "logs"+legacyBackupSuffix)
	blobsBackup := filepath.Join(dir, "blobs"+legacyBackupSuffix)

	// Resume a run that archived the dirs but crashed before finishing: the
	// archive already passed the gate on the first open, so re-scan and drain.
	if _, err := os.Stat(logsBackup); err == nil {
		payloads, serr := scanLegacyPayloads(logsBackup)
		if serr != nil {
			return nil, serr
		}
		return &legacyMigration{logsBackup: logsBackup, blobsBackup: blobsBackup, payloads: payloads}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Fresh legacy layout?
	legacy, err := hasLegacyLocalLayout(dir)
	if err != nil {
		return nil, err
	}
	if !legacy {
		return nil, nil
	}

	logsDir := filepath.Join(dir, "logs")
	blobsDir := filepath.Join(dir, "blobs")
	// Blob data with no logs to cover it is rolled-up-and-compacted bytes whose
	// only index is gone: unrecoverable. Refuse rather than serve zeros.
	if dirHasEntries(blobsDir) && !dirHasEntries(logsDir) {
		return nil, refuseLegacyLocalOnly(dir, "blob data has no covering append logs")
	}
	migratable, err := legacyLogsMigratable(logsDir)
	if err != nil {
		return nil, err
	}
	if !migratable {
		return nil, refuseLegacyLocalOnly(dir, "an append log was compacted, so some bytes live only in unrecoverable blobs")
	}

	if err := archiveLegacyLayout(dir); err != nil {
		return nil, err
	}
	payloads, err := scanLegacyPayloads(logsBackup)
	if err != nil {
		return nil, err
	}
	return &legacyMigration{logsBackup: logsBackup, blobsBackup: blobsBackup, payloads: payloads}, nil
}

// refuseLegacyLocalOnly builds the guardrail error that keeps the legacy bytes
// on disk when a local-only layout cannot be safely auto-migrated.
func refuseLegacyLocalOnly(dir, why string) error {
	return fmt.Errorf("%w: %q holds pre-journal local-only data that cannot be auto-migrated (%s); "+
		"the bytes are intact on disk — restore from a pre-upgrade backup or migrate manually", ErrLegacyLocalFormat, dir, why)
}

// materialize replays payloadID's archived log into the journal exactly once.
// It is a no-op once the migration is done or for a payload with no archived
// log. Reconstruction uses a background context so a caller's read deadline
// cannot leave a payload half-drained.
func (m *legacyMigration) materialize(payloadID string) error {
	if m.done.Load() {
		return nil
	}
	logPath, ok := m.payloads[payloadID]
	if !ok {
		return nil
	}
	oi, _ := m.onces.LoadOrStore(payloadID, &legacyMigrateOnce{})
	mo := oi.(*legacyMigrateOnce)
	mo.once.Do(func() {
		ctx := context.Background()
		fid := journal.FileID(payloadID)
		mo.err = replayLegacyLog(logPath, func(off uint64, payload []byte) error {
			return m.store.WriteAt(ctx, fid, int64(off), payload)
		})
		if mo.err == nil {
			mo.err = m.store.Commit(ctx, fid)
		}
	})
	return mo.err
}

// pendingPayloads returns every payloadID the migration must drain.
func (m *legacyMigration) pendingPayloads() []string {
	out := make([]string, 0, len(m.payloads))
	for id := range m.payloads {
		out = append(out, id)
	}
	return out
}

// finish marks the migration complete and deletes the archived legacy dirs.
// Callers must have drained every payload first. done is set before removal so
// concurrent reads stop faulting in.
func (m *legacyMigration) finish() error {
	m.done.Store(true)
	if err := os.RemoveAll(m.logsBackup); err != nil {
		return err
	}
	return os.RemoveAll(m.blobsBackup)
}

// --- FSStore surface used by the shares service to drive the drain ---

// MigratedFromLegacyLocalOnly reports whether opening this store archived (or
// resumed) a pre-journal local-only layout that a background drain must finish.
func (s *FSStore) MigratedFromLegacyLocalOnly() bool { return s.legacyMig != nil }

// LegacyPendingPayloads lists the payloads the background drain must materialize.
func (s *FSStore) LegacyPendingPayloads() []string {
	if s.legacyMig == nil {
		return nil
	}
	return s.legacyMig.pendingPayloads()
}

// MaterializeLegacyPayload drains one payload's archived log into the journal.
// Idempotent and safe to call from the background drain and a faulting read
// concurrently.
func (s *FSStore) MaterializeLegacyPayload(payloadID string) error {
	if s.legacyMig == nil {
		return nil
	}
	return s.legacyMig.materialize(payloadID)
}

// FinishLegacyMigration deletes the archived legacy dirs after a full drain.
func (s *FSStore) FinishLegacyMigration() error {
	if s.legacyMig == nil {
		return nil
	}
	return s.legacyMig.finish()
}
