package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// --- fakes ---

// fakeSource is a backup.Backupable test double.
//
//   - payload    : bytes written via w.Write when Backup is called
//   - ids        : PayloadIDSet returned on successful Backup
//   - returnErr  : forces Backup to return (nil, returnErr) without writing
//   - abortAfter : if > 0 and < len(payload), writes that many bytes then
//     returns (nil, context.Canceled) — simulates a ctx-cancel mid-stream
type fakeSource struct {
	payload    []byte
	ids        backup.PayloadIDSet
	abortAfter int
	returnErr  error
}

func (f *fakeSource) Backup(ctx context.Context, w io.Writer) (backup.PayloadIDSet, error) {
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	if f.abortAfter > 0 && f.abortAfter < len(f.payload) {
		_, _ = w.Write(f.payload[:f.abortAfter])
		return nil, context.Canceled
	}
	if _, err := w.Write(f.payload); err != nil {
		return nil, err
	}
	return f.ids, nil
}

func (f *fakeSource) Restore(ctx context.Context, r io.Reader) error { return nil }

// fakeDest is a destination.Destination test double. It records the manifest
// pointer it received, the full payload bytes, and applies driver-simulated
// SHA256 + SizeBytes onto the manifest on success.
type fakeDest struct {
	putErr      error
	setSHA256   string
	setSize     int64
	gotManifest *manifest.Manifest
	gotPayload  []byte
}

func (d *fakeDest) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	buf, readErr := io.ReadAll(payload)
	d.gotPayload = buf
	if d.putErr != nil {
		return d.putErr
	}
	if readErr != nil {
		return readErr
	}
	m.SHA256 = d.setSHA256
	m.SizeBytes = d.setSize
	d.gotManifest = m
	return nil
}

func (d *fakeDest) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	return nil, nil, nil
}

func (d *fakeDest) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	return nil, nil
}

func (d *fakeDest) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *fakeDest) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *fakeDest) Delete(ctx context.Context, id string) error { return nil }
func (d *fakeDest) ValidateConfig(ctx context.Context) error    { return nil }
func (d *fakeDest) Close() error                                { return nil }

// fakeStore is an in-memory JobStore test double. All mutations are recorded
// for post-hoc assertion.
type fakeStore struct {
	createdJobs    []models.BackupJob
	updatedJobs    []models.BackupJob
	createdRecords []models.BackupRecord
	createJobErr   error
	createRecErr   error
}

func (s *fakeStore) CreateBackupJob(ctx context.Context, j *models.BackupJob) (string, error) {
	if s.createJobErr != nil {
		return "", s.createJobErr
	}
	s.createdJobs = append(s.createdJobs, *j)
	return j.ID, nil
}

func (s *fakeStore) UpdateBackupJob(ctx context.Context, j *models.BackupJob) error {
	s.updatedJobs = append(s.updatedJobs, *j)
	return nil
}

func (s *fakeStore) CreateBackupRecord(ctx context.Context, r *models.BackupRecord) (string, error) {
	if s.createRecErr != nil {
		return "", s.createRecErr
	}
	s.createdRecords = append(s.createdRecords, *r)
	return r.ID, nil
}

// fixedClock returns the same time on every call.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// --- helpers ---

func newRepo(encrypt bool, keyRef string) *models.BackupRepo {
	return &models.BackupRepo{
		ID:                "repo-1",
		TargetID:          "store-xyz",
		TargetKind:        "metadata",
		Name:              "test-repo",
		Kind:              models.BackupRepoKindLocal,
		EncryptionEnabled: encrypt,
		EncryptionKeyRef:  keyRef,
	}
}

func newPayloadSet(ids ...string) backup.PayloadIDSet {
	s := backup.NewPayloadIDSet()
	for _, id := range ids {
		s.Add(id)
	}
	return s
}

// --- tests ---

// T1 happy path.
func TestRunBackup_HappyPath(t *testing.T) {
	src := &fakeSource{
		payload: []byte("hello"),
		ids:     newPayloadSet("p1"),
	}
	dst := &fakeDest{setSHA256: "abc", setSize: 5}
	store := &fakeStore{}
	repo := newRepo(false, "")

	clk := fixedClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e := New(store, clk)

	rec, err := e.RunBackup(context.Background(), src, dst, repo, "store-xyz", "memory")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, rec.ID, dst.gotManifest.BackupID)
	require.Equal(t, "abc", rec.SHA256)
	require.Equal(t, int64(5), rec.SizeBytes)
	require.Equal(t, models.BackupStatusSucceeded, rec.Status)
	require.Equal(t, "store-xyz", rec.StoreID)
	require.Equal(t, "repo-1", rec.RepoID)
}

// T2 ULID single source of truth: one ID in three places.
func TestRunBackup_ULIDIdentity(t *testing.T) {
	src := &fakeSource{payload: []byte("x"), ids: newPayloadSet("p1")}
	dst := &fakeDest{setSHA256: "h", setSize: 1}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	rec, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.NoError(t, err)

	require.Equal(t, rec.ID, dst.gotManifest.BackupID, "manifest BackupID must equal record ID")
	// Final updated job (index len-1) must carry BackupRecordID = recordID.
	require.NotEmpty(t, store.updatedJobs, "expected at least one job update")
	finalJob := store.updatedJobs[len(store.updatedJobs)-1]
	require.NotNil(t, finalJob.BackupRecordID)
	require.Equal(t, rec.ID, *finalJob.BackupRecordID, "job.BackupRecordID must equal record ID")

	// BackupJob.ID is a distinct ULID per D-20.
	require.NotEmpty(t, store.createdJobs)
	require.NotEqual(t, rec.ID, store.createdJobs[0].ID, "job ID must differ from record ID (D-20)")
}

// T3 job row lifecycle on happy path: running → succeeded.
func TestRunBackup_JobLifecycleHappyPath(t *testing.T) {
	src := &fakeSource{payload: []byte("x"), ids: newPayloadSet("p1")}
	dst := &fakeDest{setSHA256: "h", setSize: 1}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	_, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.NoError(t, err)

	require.Len(t, store.createdJobs, 1)
	j := store.createdJobs[0]
	require.Equal(t, models.BackupStatusRunning, j.Status)
	require.NotNil(t, j.StartedAt)
	require.Equal(t, models.BackupJobKindBackup, j.Kind)
	require.Equal(t, repo.ID, j.RepoID)

	require.NotEmpty(t, store.updatedJobs)
	final := store.updatedJobs[len(store.updatedJobs)-1]
	require.Equal(t, models.BackupStatusSucceeded, final.Status)
	require.NotNil(t, final.FinishedAt)
	require.NotNil(t, final.BackupRecordID)
}

// T4 destination failure: no BackupRecord created.
func TestRunBackup_DestinationFailure(t *testing.T) {
	src := &fakeSource{payload: []byte("x"), ids: newPayloadSet("p1")}
	dst := &fakeDest{putErr: errors.New("boom: destination unavailable")}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	rec, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.Error(t, err)
	require.Nil(t, rec)
	require.Empty(t, store.createdRecords, "no BackupRecord should be created on destination failure (D-16)")

	// Job should be updated to failed.
	require.NotEmpty(t, store.updatedJobs)
	final := store.updatedJobs[len(store.updatedJobs)-1]
	require.Equal(t, models.BackupStatusFailed, final.Status)
	require.NotEmpty(t, final.Error)
}

// T5 source failure: no BackupRecord created; job failed.
func TestRunBackup_SourceFailure(t *testing.T) {
	src := &fakeSource{returnErr: backup.ErrBackupAborted}
	dst := &fakeDest{}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	rec, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.Error(t, err)
	require.True(t, errors.Is(err, backup.ErrBackupAborted), "err should wrap ErrBackupAborted")
	require.Nil(t, rec)
	require.Empty(t, store.createdRecords, "no BackupRecord on source failure (D-16)")

	require.NotEmpty(t, store.updatedJobs)
	final := store.updatedJobs[len(store.updatedJobs)-1]
	// ErrBackupAborted maps to interrupted (D-18).
	require.Equal(t, models.BackupStatusInterrupted, final.Status)
}

// T6 ctx cancellation mid-stream: job ends interrupted.
func TestRunBackup_ContextCancelled(t *testing.T) {
	src := &fakeSource{
		payload:    []byte("this-is-a-long-payload-that-we-interrupt"),
		abortAfter: 10,
		ids:        newPayloadSet("p1"),
	}
	dst := &fakeDest{}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	rec, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.Error(t, err)
	require.Nil(t, rec)
	require.Empty(t, store.createdRecords)

	require.NotEmpty(t, store.updatedJobs)
	final := store.updatedJobs[len(store.updatedJobs)-1]
	require.Equal(t, models.BackupStatusInterrupted, final.Status, "ctx cancellation → interrupted (D-18)")
}

// T7 pipe plumbing: destination receives the EXACT payload bytes.
func TestRunBackup_PipePlumbingBytesExact(t *testing.T) {
	payload := make([]byte, 1<<20) // 1 MiB
	_, err := rand.Read(payload)
	require.NoError(t, err)

	src := &fakeSource{payload: payload, ids: newPayloadSet("p1")}
	dst := &fakeDest{setSHA256: "h", setSize: int64(len(payload))}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	_, err = e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.NoError(t, err)

	require.True(t, bytes.Equal(payload, dst.gotPayload), "destination must receive source bytes exactly")
}

// T8 manifest fields populated correctly.
func TestRunBackup_ManifestFields(t *testing.T) {
	src := &fakeSource{payload: []byte("x"), ids: newPayloadSet("p1", "p2")}
	dst := &fakeDest{setSHA256: "h", setSize: 1}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	start := time.Now().UTC().Add(-1 * time.Second)
	_, err := e.RunBackup(context.Background(), src, dst, repo, "store-xyz", "badger")
	require.NoError(t, err)
	end := time.Now().UTC().Add(1 * time.Second)

	m := dst.gotManifest
	require.NotNil(t, m)
	require.Equal(t, manifest.CurrentVersion, m.ManifestVersion)
	require.NotEmpty(t, m.BackupID)
	require.True(t, !m.CreatedAt.Before(start) && !m.CreatedAt.After(end), "CreatedAt within test window")
	require.Equal(t, "store-xyz", m.StoreID)
	require.Equal(t, "badger", m.StoreKind)
	require.Equal(t, []string{"p1", "p2"}, m.PayloadIDSet, "PayloadIDSet must be populated + sorted after success")
}

// T9 encryption KeyRef propagation.
func TestRunBackup_EncryptionEnabled(t *testing.T) {
	src := &fakeSource{payload: []byte("x"), ids: newPayloadSet("p1")}
	dst := &fakeDest{setSHA256: "h", setSize: 1}
	store := &fakeStore{}
	repo := newRepo(true, "env:TESTKEY")

	e := New(store, nil)
	_, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.NoError(t, err)

	m := dst.gotManifest
	require.NotNil(t, m)
	require.True(t, m.Encryption.Enabled)
	require.Equal(t, "aes-256-gcm", m.Encryption.Algorithm)
	require.Equal(t, "env:TESTKEY", m.Encryption.KeyRef)
}

// T10 encryption disabled: algorithm + key_ref empty.
func TestRunBackup_EncryptionDisabled(t *testing.T) {
	src := &fakeSource{payload: []byte("x"), ids: newPayloadSet("p1")}
	dst := &fakeDest{setSHA256: "h", setSize: 1}
	store := &fakeStore{}
	repo := newRepo(false, "")

	e := New(store, nil)
	_, err := e.RunBackup(context.Background(), src, dst, repo, "s1", "memory")
	require.NoError(t, err)

	m := dst.gotManifest
	require.NotNil(t, m)
	require.False(t, m.Encryption.Enabled)
	require.Empty(t, m.Encryption.Algorithm)
	require.Empty(t, m.Encryption.KeyRef)
}

// Defensive: nil args must be rejected before doing anything.
func TestRunBackup_NilGuards(t *testing.T) {
	store := &fakeStore{}
	e := New(store, nil)

	_, err := e.RunBackup(context.Background(), nil, &fakeDest{}, newRepo(false, ""), "s", "memory")
	require.Error(t, err)

	_, err = e.RunBackup(context.Background(), &fakeSource{}, nil, newRepo(false, ""), "s", "memory")
	require.Error(t, err)

	_, err = e.RunBackup(context.Background(), &fakeSource{}, &fakeDest{}, nil, "s", "memory")
	require.Error(t, err)
}
