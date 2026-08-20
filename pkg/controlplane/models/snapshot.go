package models

import (
	"errors"
	"path/filepath"
	"time"
)

// Snapshot lifecycle states. State machine:
//
//	creating -> ready
//	creating -> failed
//	failed   -> creating   (retry; failed is not terminal)
const (
	StateCreating string = "creating"
	StateReady    string = "ready"
	StateFailed   string = "failed"
)

// Snapshot is the persisted record of a per-share point-in-time snapshot.
// The partial unique index idx_share_creating enforces at most one
// in-flight snapshot per share.
type Snapshot struct {
	ID             string `gorm:"primaryKey;size:36" json:"id"`
	Name           string `gorm:"size:255" json:"name,omitempty"`
	ShareName      string `gorm:"index;not null;size:255;index:idx_share_creating,where:state='creating',unique" json:"share_name"`
	State          string `gorm:"not null;size:20;default:'creating'" json:"state"`
	MetadataEngine string `gorm:"not null;size:20" json:"metadata_engine"`
	ManifestCount  int64  `gorm:"not null;default:0" json:"manifest_count"`
	RemoteDurable  bool   `gorm:"not null;default:false" json:"remote_durable"`
	// JournalVersion is the local-journal LSN watermark captured when the
	// snapshot became ready. A local-only restore rewinds the journal to it
	// (RestoreToVersion), and the share derives its GC pin as the max over live
	// snapshots. 0 for pre-#1718 rows and remote-only shares that never pin.
	JournalVersion uint64 `gorm:"not null;default:0" json:"journal_version"`
	// Scheduled marks snapshots created by the background snapshot scheduler.
	// Only scheduled snapshots are eligible for automatic retention pruning;
	// manually-created snapshots are never auto-pruned.
	Scheduled bool   `gorm:"not null;default:false" json:"scheduled"`
	Error     string `gorm:"size:1024" json:"error,omitempty"`
	// FailureKind records which sentinel produced Error. It lets a caller
	// that arrives after the orchestration goroutine has exited rebuild a
	// typed error from the row instead of matching the message text. Empty
	// on rows that never failed and on rows written before the kind was
	// recorded.
	FailureKind string    `gorm:"size:32" json:"failure_kind,omitempty"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// Snapshot failure kinds, as persisted on the row's FailureKind column.
// These tokens are part of the stored format: renaming one strands the rows
// already written with the old spelling.
const (
	FailureKindBackup       string = "backup"
	FailureKindVerify       string = "verify"
	FailureKindDrainTimeout string = "drain_timeout"
)

// snapshotFailureSentinels pairs each failure kind with the sentinel it
// records, in classification order.
var snapshotFailureSentinels = []struct {
	kind     string
	sentinel error
}{
	{FailureKindDrainTimeout, ErrSnapshotDrainTimeout},
	{FailureKindVerify, ErrSnapshotVerifyFailed},
	{FailureKindBackup, ErrSnapshotBackupFailed},
}

// SnapshotFailureKind classifies cause into the token stored on the row.
// Returns "" when cause wraps no known snapshot failure sentinel.
func SnapshotFailureKind(cause error) string {
	for _, m := range snapshotFailureSentinels {
		if errors.Is(cause, m.sentinel) {
			return m.kind
		}
	}
	return ""
}

// snapshotFailure is a rebuilt failure: it prints the message persisted on the
// row and unwraps to the sentinel the row's kind names. The persisted message
// is the original error's own Error() output, which already contains the
// sentinel's text, so printing it verbatim rather than re-wrapping keeps the
// rebuilt error's string identical to the one an in-memory waiter sees.
type snapshotFailure struct {
	msg      string
	sentinel error
}

func (e *snapshotFailure) Error() string { return e.msg }
func (e *snapshotFailure) Unwrap() error { return e.sentinel }

// SnapshotFailureError rebuilds a caller-facing error from a terminal
// state='failed' row. It carries the persisted message and, when the row
// records a known failure kind, unwraps to the matching sentinel so errors.Is
// classifies it exactly as the in-memory error would have. Rows with an empty
// or unrecognised kind yield a plain error — non-nil either way, so a failed
// snapshot is never mistaken for a successful one.
func SnapshotFailureError(s *Snapshot) error {
	msg := s.Error
	if msg == "" {
		msg = "snapshot failed"
	}
	for _, m := range snapshotFailureSentinels {
		if m.kind == s.FailureKind {
			return &snapshotFailure{msg: msg, sentinel: m.sentinel}
		}
	}
	return errors.New(msg)
}

func (Snapshot) TableName() string {
	return "snapshots"
}

func (s *Snapshot) SnapshotDir(shareDataDir string) string {
	return filepath.Join(shareDataDir, "snapshots", s.ID)
}

func (s *Snapshot) ManifestPath(shareDataDir string) string {
	return filepath.Join(s.SnapshotDir(shareDataDir), "manifest.hashes")
}

func (s *Snapshot) MetadataDumpPath(shareDataDir string) string {
	return filepath.Join(s.SnapshotDir(shareDataDir), "metadata.dump")
}
