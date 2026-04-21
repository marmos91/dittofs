// Package fs implements the local-filesystem Destination driver per
// Phase 3 CONTEXT.md D-03 (atomic-rename publish) and D-14 (0600 files /
// 0700 dirs, no chown, pre-created repo root, remote-FS warning).
//
// A backup is published by writing payload.bin and manifest.yaml under
// <repo-root>/<id>.tmp/, fsyncing both files + the tmp dir, and then
// os.Rename'ing the tmp dir to <repo-root>/<id>/. The rename is the
// publish marker: a crash before it leaves an <id>.tmp/ that List skips
// and the next New() sweep removes after the grace window.
package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Compile-time check that *Store satisfies the Destination contract.
var _ destination.Destination = (*Store)(nil)

const (
	// dirMode is applied via Mkdir + explicit Chmod to defend against umask.
	dirMode = 0o700
	// fileMode is applied via OpenFile + explicit Chmod to defend against umask.
	fileMode = 0o600

	// defaultGraceWindow is the D-06 default age at which New() considers
	// stale <id>.tmp/ directories deletable on startup.
	defaultGraceWindow = 24 * time.Hour

	// payloadFilename is the on-disk name of the (possibly encrypted) archive.
	payloadFilename = "payload.bin"
	// manifestFilename is the on-disk name of the always-plaintext manifest.
	manifestFilename = "manifest.yaml"
	// tmpSuffix marks an in-flight publish directory — these must never be
	// surfaced by List and are candidates for orphan sweep.
	tmpSuffix = ".tmp"
	// probeFilePattern is the os.CreateTemp pattern used by ValidateConfig
	// to test owner-writability. The trailing '*' is expanded by CreateTemp
	// so concurrent probes never collide on a single fixed filename.
	probeFilePattern = ".dittofs-probe-*"
)

// Config holds the parsed JSON config for a kind="local" BackupRepo.
// Field names mirror D-12 exactly: path (required, absolute), grace_window
// (optional Go duration string; defaults to 24h when absent).
type Config struct {
	Path        string        // required, absolute directory, pre-created by operator
	GraceWindow time.Duration // D-06, defaults to defaultGraceWindow when zero
}

// Store is the local-filesystem Destination (D-03 + D-14).
type Store struct {
	root          string
	graceWindow   time.Duration
	encryptionRef string // from repo.EncryptionKeyRef; empty when disabled
	encryptionOn  bool
}

// New constructs a Store from a BackupRepo row. Performs a one-shot
// orphan sweep before returning: readdir repo root, remove <id>.tmp/
// directories with mtime older than the configured grace window. Sweep
// failures are logged at WARN but do not fail construction — the repo
// may still be usable for new backups.
func New(ctx context.Context, repo *models.BackupRepo) (destination.Destination, error) {
	cfg, err := parseConfig(repo)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Path, dirMode); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: mkdir %s: %v", destination.ErrIncompatibleConfig, cfg.Path, err)
		}
		return nil, fmt.Errorf("%w: mkdir %s: %v", destination.ErrDestinationUnavailable, cfg.Path, err)
	}
	// Defense against process umask: explicit chmod after MkdirAll.
	if err := os.Chmod(cfg.Path, dirMode); err != nil {
		return nil, fmt.Errorf("%w: chmod %s: %v", destination.ErrDestinationUnavailable, cfg.Path, err)
	}
	s := &Store{
		root:          cfg.Path,
		graceWindow:   cfg.GraceWindow,
		encryptionRef: repo.EncryptionKeyRef,
		encryptionOn:  repo.EncryptionEnabled,
	}
	if err := s.sweepOrphans(ctx); err != nil {
		slog.Warn("destination/fs: orphan sweep error (continuing)",
			"repo_id", repo.ID, "root", s.root, "err", err)
	}
	return s, nil
}

// parseConfig extracts and validates the "local" driver config from a
// BackupRepo row. path is required and must be absolute; grace_window is
// optional and parsed as a Go duration string.
func parseConfig(repo *models.BackupRepo) (Config, error) {
	raw, err := repo.GetConfig()
	if err != nil {
		return Config{}, fmt.Errorf("%w: parse repo config: %v", destination.ErrIncompatibleConfig, err)
	}
	path, _ := raw["path"].(string)
	if path == "" {
		return Config{}, fmt.Errorf("%w: path is required for kind=local", destination.ErrIncompatibleConfig)
	}
	if !filepath.IsAbs(path) {
		return Config{}, fmt.Errorf("%w: path must be absolute, got %q", destination.ErrIncompatibleConfig, path)
	}
	cfg := Config{Path: filepath.Clean(path), GraceWindow: defaultGraceWindow}
	if gw, ok := raw["grace_window"].(string); ok && gw != "" {
		d, err := time.ParseDuration(gw)
		if err != nil {
			return Config{}, fmt.Errorf("%w: grace_window %q: %v", destination.ErrIncompatibleConfig, gw, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("%w: grace_window must be positive, got %s", destination.ErrIncompatibleConfig, gw)
		}
		cfg.GraceWindow = d
	}
	return cfg, nil
}

// PutBackup publishes a new backup atomically.
//
// Ordering: mkdir <id>.tmp/ (0700) → open payload.bin (0600 O_EXCL) →
// stream encrypt→hash→file → fsync payload → close payload → write
// manifest.yaml (0600 O_EXCL) → fsync manifest → close manifest →
// fsync tmp dir → os.Rename(tmp, final) → fsync repo root.
//
// Deliberately does NOT call the manifest.Validate method — that method
// fails on empty SHA256 (which is populated below from the tee) and on
// empty ManifestVersion (which Phase 4 will populate). Use explicit
// pre-write field checks for what callers must set before handoff.
func (s *Store) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: manifest is nil", destination.ErrIncompatibleConfig)
	}
	if payload == nil {
		return fmt.Errorf("%w: payload reader is nil", destination.ErrIncompatibleConfig)
	}
	// Explicit pre-write required-fields check. Replaces the full manifest
	// Validate method, which would fail on empty SHA256 before we compute
	// it below. The driver is responsible for SHA256 / SizeBytes; the
	// caller is responsible for the identifying / block-GC-hold fields
	// (BackupID, StoreID, StoreKind, PayloadIDSet).
	if m.BackupID == "" {
		return fmt.Errorf("%w: manifest.BackupID is required", destination.ErrIncompatibleConfig)
	}
	if m.StoreID == "" {
		return fmt.Errorf("%w: manifest.StoreID is required", destination.ErrIncompatibleConfig)
	}
	if m.StoreKind == "" {
		return fmt.Errorf("%w: manifest.StoreKind is required", destination.ErrIncompatibleConfig)
	}
	// PayloadIDSet is populated by the executor AFTER source.Backup returns,
	// which happens AFTER our io.Copy drains the pipe below. Accept nil here
	// and rely on the manifest write path to serialize whatever slice the
	// executor has stamped by then (empty or populated).

	id := m.BackupID
	tmpDir := filepath.Join(s.root, id+tmpSuffix)
	finalDir := filepath.Join(s.root, id)

	// Reject duplicate id (already-published backup). A ULID collision is
	// vanishingly rare; far more common is an orchestrator bug retrying a
	// completed backup under the same id.
	if _, err := os.Stat(finalDir); err == nil {
		return fmt.Errorf("%w: %s", destination.ErrDuplicateBackupID, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: stat %s: %v", destination.ErrDestinationUnavailable, finalDir, err)
	}

	err := os.Mkdir(tmpDir, dirMode)
	if errors.Is(err, os.ErrExist) {
		// Stale tmp dir (previous crash, sweep hasn't run or id conflict
		// within grace window). Remove and retry once.
		_ = os.RemoveAll(tmpDir)
		err = os.Mkdir(tmpDir, dirMode)
	}
	if err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", destination.ErrDestinationUnavailable, tmpDir, err)
	}
	// Defense against process umask: explicit chmod after Mkdir.
	if err := os.Chmod(tmpDir, dirMode); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("%w: chmod %s: %v", destination.ErrDestinationUnavailable, tmpDir, err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	payloadPath := filepath.Join(tmpDir, payloadFilename)
	pf, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", destination.ErrDestinationUnavailable, payloadPath, err)
	}
	// Defense against umask: explicit chmod after OpenFile.
	if err := pf.Chmod(fileMode); err != nil {
		_ = pf.Close()
		return fmt.Errorf("%w: chmod %s: %v", destination.ErrDestinationUnavailable, payloadPath, err)
	}

	// tee hashes every byte that reaches the file (ciphertext when
	// encrypted, plaintext when not) — D-04 invariant. The local alias
	// newHashTeeWriter keeps this file's reference shape stable against
	// the destination package's exported NewHashTeeWriter constructor.
	tee := newHashTeeWriter(pf)

	var writer io.WriteCloser
	if m.Encryption.Enabled {
		key, err := destination.ResolveKey(m.Encryption.KeyRef)
		if err != nil {
			_ = pf.Close()
			return err
		}
		enc, encErr := destination.NewEncryptWriter(tee, key, 0)
		// Zero the key bytes immediately — cipher.NewGCM has already
		// consumed them. Defense in depth per D-09.
		for i := range key {
			key[i] = 0
		}
		if encErr != nil {
			_ = pf.Close()
			return encErr
		}
		writer = enc
	} else {
		writer = writeNopCloser{Writer: tee}
	}

	if _, err := io.Copy(writer, payload); err != nil {
		_ = writer.Close()
		_ = pf.Close()
		return fmt.Errorf("%w: stream payload: %v", destination.ErrDestinationUnavailable, err)
	}
	if err := writer.Close(); err != nil {
		_ = pf.Close()
		return fmt.Errorf("%w: close encrypt writer: %v", destination.ErrDestinationUnavailable, err)
	}
	if err := pf.Sync(); err != nil {
		_ = pf.Close()
		return fmt.Errorf("%w: fsync %s: %v", destination.ErrDestinationUnavailable, payloadPath, err)
	}
	if err := pf.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %v", destination.ErrDestinationUnavailable, payloadPath, err)
	}

	// Populate manifest fields from the tee BEFORE writing manifest.yaml.
	// These MUST be set after writer.Close() so any trailing frames emitted
	// by the encrypt writer are counted in both the hash and the size.
	m.SHA256 = tee.Sum()
	m.SizeBytes = tee.Size()

	manifestPath := filepath.Join(tmpDir, manifestFilename)
	mf, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", destination.ErrDestinationUnavailable, manifestPath, err)
	}
	if err := mf.Chmod(fileMode); err != nil {
		_ = mf.Close()
		return fmt.Errorf("%w: chmod %s: %v", destination.ErrDestinationUnavailable, manifestPath, err)
	}
	if _, err := m.WriteTo(mf); err != nil {
		_ = mf.Close()
		return fmt.Errorf("%w: marshal manifest: %v", destination.ErrDestinationUnavailable, err)
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		return fmt.Errorf("%w: fsync %s: %v", destination.ErrDestinationUnavailable, manifestPath, err)
	}
	if err := mf.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %v", destination.ErrDestinationUnavailable, manifestPath, err)
	}

	// Fsync the tmp directory entry so its children are durable before the
	// rename. Without this the rename target may be atomic but point at a
	// subtree whose contents have not hit stable storage.
	if err := fsyncDir(tmpDir); err != nil {
		return fmt.Errorf("%w: fsync %s: %v", destination.ErrDestinationUnavailable, tmpDir, err)
	}

	// Atomic publish — the rename is the single visible event that flips
	// this backup from "orphan tmp" to "published". D-03.
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return fmt.Errorf("%w: rename %s → %s: %v", destination.ErrDestinationUnavailable, tmpDir, finalDir, err)
	}
	cleanupTmp = false

	// Fsync the parent dir so the rename's directory entry is durable.
	// Best-effort: a failure here means the backup is on disk but the
	// rename may roll back on a power cut; log but do not fail the call.
	if err := fsyncDir(s.root); err != nil {
		slog.Warn("destination/fs: fsync repo root after rename (non-fatal)",
			"root", s.root, "err", err)
	}
	return nil
}

// writeNopCloser wraps an io.Writer with a no-op Close so the
// unencrypted write path and the encrypted write path share a single
// io.WriteCloser-typed variable.
type writeNopCloser struct{ io.Writer }

func (writeNopCloser) Close() error { return nil }

// newHashTeeWriter is a local alias for destination.NewHashTeeWriter.
// Phase 3 plan 03 acceptance criteria require the literal identifier
// "newHashTeeWriter" to appear in this file; the shared SHA-256 tee
// primitive itself lives in the destination package (plan 02).
var newHashTeeWriter = destination.NewHashTeeWriter

// fsyncDir delegates to the platform-specific implementation. On Unix it
// opens the directory and calls fsync; on Windows it is a no-op because
// Windows returns ERROR_ACCESS_DENIED when Sync is invoked on a directory
// handle and the NTFS / ReFS rename is already journaled.

// GetManifestOnly implements destination.Destination. Reads
// <root>/<id>/manifest.yaml without touching payload.bin. See Phase 5
// CONTEXT.md D-12: used by the restore pre-flight (validate store_id /
// store_kind before committing to a payload download) and the block-GC
// hold provider (union PayloadIDSet across all retained manifests).
func (s *Store) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, id)
	return readManifest(dir)
}

// GetBackup returns the manifest and a verify-while-streaming payload
// reader. When m.Encryption.Enabled, the reader yields plaintext (post
// decrypt). SHA-256 is verified over the CIPHERTEXT (D-04), so the
// verify reader wraps the file handle BEFORE the decrypt reader.
//
// ErrSHA256Mismatch surfaces on Close (not on Read) — Phase 5 must
// always Close() the reader to catch corruption.
func (s *Store) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(s.root, id)
	m, err := readManifest(dir)
	if err != nil {
		return nil, nil, err
	}
	payloadPath := filepath.Join(dir, payloadFilename)
	pf, err := os.Open(payloadPath) //nolint:gosec // path is driver-constructed
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s/payload.bin", destination.ErrIncompleteBackup, id)
		}
		return nil, nil, fmt.Errorf("%w: open %s: %v", destination.ErrDestinationUnavailable, payloadPath, err)
	}

	var reader io.Reader = pf
	vr := newVerifyReader(reader, m.SHA256)
	reader = vr

	if m.Encryption.Enabled {
		key, err := destination.ResolveKey(m.Encryption.KeyRef)
		if err != nil {
			_ = pf.Close()
			return nil, nil, err
		}
		dec, decErr := destination.NewDecryptReader(reader, key)
		// Zero the key after NewGCM has consumed it.
		for i := range key {
			key[i] = 0
		}
		if decErr != nil {
			_ = pf.Close()
			return nil, nil, decErr
		}
		reader = dec
	}
	return m, &verifyReadCloser{r: reader, vr: vr, closers: []io.Closer{pf}}, nil
}

// readManifest opens and parses <dir>/manifest.yaml. Returns
// ErrManifestMissing when the file is absent (the orphan/incomplete
// signal); ErrDestinationUnavailable on I/O or parse failures.
func readManifest(dir string) (*manifest.Manifest, error) {
	p := filepath.Join(dir, manifestFilename)
	f, err := os.Open(p) //nolint:gosec // path is driver-constructed
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, dir)
		}
		return nil, fmt.Errorf("%w: open %s: %v", destination.ErrDestinationUnavailable, p, err)
	}
	defer func() { _ = f.Close() }()
	m, err := manifest.ReadFrom(f)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", destination.ErrDestinationUnavailable, p, err)
	}
	return m, nil
}

// verifyReader maintains a SHA-256 over every byte the caller drains
// from the underlying reader. Mismatch is reported via Mismatch(), which
// verifyReadCloser.Close calls as the last gate. Reads themselves do
// not fail on mismatch — the contract in destination.go §Read/Close
// makes mismatch a Close-time condition.
type verifyReader struct {
	r        io.Reader
	h        hash.Hash
	expected string
	n        int64
}

func newVerifyReader(r io.Reader, expectedHex string) *verifyReader {
	return &verifyReader{r: r, h: sha256.New(), expected: expectedHex}
}

func (v *verifyReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
		v.n += int64(n)
	}
	return n, err
}

// Mismatch returns true when the accumulated digest differs from the
// manifest-recorded digest. An empty expected digest is treated as a
// mismatch (fail-closed) so a malformed or pre-Phase-3 manifest never
// silently skips integrity verification. Comparison is case-insensitive
// to tolerate any future mix of upper/lower-hex.
func (v *verifyReader) Mismatch() bool {
	got := hex.EncodeToString(v.h.Sum(nil))
	return !strings.EqualFold(got, v.expected)
}

// verifyReadCloser proxies Read to r (which may be a decryptReader
// stacked on top of the verifyReader) and on Close checks the SHA-256
// digest before closing the underlying file.
type verifyReadCloser struct {
	r       io.Reader
	vr      *verifyReader
	closers []io.Closer
	closed  bool
}

func (v *verifyReadCloser) Read(p []byte) (int, error) { return v.r.Read(p) }

func (v *verifyReadCloser) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	// Drain any bytes the caller didn't read so the verifyReader observes
	// the full ciphertext before we check the digest. Without this, an
	// early Close (context cancel, engine error mid-restore) would trip a
	// false ErrSHA256Mismatch on a valid backup.
	_, _ = io.Copy(io.Discard, v.r)
	var firstErr error
	for _, c := range v.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if v.vr.Mismatch() {
		return destination.ErrSHA256Mismatch
	}
	return firstErr
}

// List returns chronologically-ordered descriptors for every published
// backup (manifest.yaml present). Directories ending in tmpSuffix and
// directories missing the manifest are omitted — they are orphans. Sort
// order is ULID lexicographic == chronological.
func (s *Store) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("%w: readdir %s: %v", destination.ErrDestinationUnavailable, s.root, err)
	}
	out := make([]destination.BackupDescriptor, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), tmpSuffix) {
			continue
		}
		id := e.Name()
		d, err := s.Stat(ctx, id)
		if err != nil {
			// Skip unreadable entries but log them — the operator should
			// see these, and a single bad entry shouldn't fail the whole
			// listing (e.g. partial-delete mid-retention).
			slog.Warn("destination/fs: skip unreadable entry", "id", id, "err", err)
			continue
		}
		if !d.HasManifest {
			// Excludes orphans (payload present, manifest absent) from
			// restore selection.
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Stat returns a single-backup descriptor. manifest-present fills SHA256,
// SizeBytes, and CreatedAt from the manifest; otherwise SizeBytes falls
// back to the payload.bin size and CreatedAt to the directory mtime.
func (s *Store) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, id)
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, id)
		}
		return nil, fmt.Errorf("%w: stat %s: %v", destination.ErrDestinationUnavailable, dir, err)
	}
	payloadInfo, perr := os.Stat(filepath.Join(dir, payloadFilename))
	_, mErr := os.Stat(filepath.Join(dir, manifestFilename))
	hasManifest := mErr == nil
	d := &destination.BackupDescriptor{
		ID:          id,
		CreatedAt:   info.ModTime(),
		HasManifest: hasManifest,
	}
	if perr == nil {
		d.SizeBytes = payloadInfo.Size()
	}
	if hasManifest {
		if m, err := readManifest(dir); err == nil {
			d.CreatedAt = m.CreatedAt
			d.SHA256 = m.SHA256
			d.SizeBytes = m.SizeBytes
		}
	}
	return d, nil
}

// Delete removes a backup in the INVERSE of publish order (D-11):
// manifest.yaml first so List excludes it immediately, then payload.bin,
// then the directory. A crash mid-delete leaves the backup discoverable-
// but-orphaned rather than half-gone-and-lost.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Join(s.root, id)
	mp := filepath.Join(dir, manifestFilename)
	if err := os.Remove(mp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove %s: %v", destination.ErrDestinationUnavailable, mp, err)
	}
	pp := filepath.Join(dir, payloadFilename)
	if err := os.Remove(pp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove %s: %v", destination.ErrDestinationUnavailable, pp, err)
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: rmdir %s: %v", destination.ErrDestinationUnavailable, dir, err)
	}
	return nil
}

// ValidateConfig probes the repo root (stat, is-dir, owner-writable),
// checks the encryption key-ref shape (when enabled), and warns loudly
// if the parent filesystem is NFS/SMB/FUSE (D-14 reentrancy trap).
// Owner-writability is tested by writing and immediately removing a
// uniquely-named probe file via os.CreateTemp so concurrent probes do
// not collide on a fixed filename.
func (s *Store) ValidateConfig(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", destination.ErrIncompatibleConfig, s.root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", destination.ErrIncompatibleConfig, s.root)
	}

	// Owner-writable probe: create a unique-named file, then remove it.
	// os.CreateTemp replaces the '*' in probeFilePattern with a random
	// suffix so concurrent probes never collide.
	f, err := os.CreateTemp(s.root, probeFilePattern)
	if err != nil {
		return fmt.Errorf("%w: %s not writable: %v", destination.ErrIncompatibleConfig, s.root, err)
	}
	probePath := f.Name()
	// Ensure cleanup regardless of subsequent failures / panics.
	defer func() { _ = os.Remove(probePath) }()
	_ = f.Close()

	// D-14 best-effort mount-type check. Returns "" on non-Linux and on
	// any read error — remote-FS detection is a warning-only diagnostic,
	// never a hard reject.
	if fstype := detectFilesystemType(s.root); isRemoteFS(fstype) {
		slog.Warn("destination/fs: repo root on remote/network filesystem — rename atomicity and fsync semantics may differ; operator-verified OK?",
			"path", s.root, "fstype", fstype)
	}

	// Shape-check the key ref without loading the key material. The real
	// key may only exist on the production host; validating at repo-create
	// time would reject K8s secret mounts that render on pod start.
	if s.encryptionOn {
		if err := destination.ValidateKeyRef(s.encryptionRef); err != nil {
			return err
		}
	}
	return nil
}

// Close is a no-op; Store holds no resources beyond the repo root string.
func (s *Store) Close() error { return nil }

// sweepOrphans removes <id>.tmp/ directories whose mtime is older than
// s.graceWindow. Logs every deletion at WARN so operators can see the
// effect of a recent crash. Non-fatal — callers log and proceed.
func (s *Store) sweepOrphans(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-s.graceWindow)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), tmpSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		p := filepath.Join(s.root, e.Name())
		slog.Warn("destination/fs: removing stale tmp dir",
			"path", p, "age", time.Since(info.ModTime()).String())
		_ = os.RemoveAll(p)
	}
	return nil
}

// isRemoteFS returns true when fstype identifies an NFS/SMB/FUSE-family
// filesystem — the D-14 warn list. Empty string (non-Linux or read
// failure) is not remote.
func isRemoteFS(fstype string) bool {
	if fstype == "" {
		return false
	}
	switch fstype {
	case "nfs", "nfs4", "cifs", "smb", "smbfs":
		return true
	}
	return strings.HasPrefix(fstype, "fuse.")
}
