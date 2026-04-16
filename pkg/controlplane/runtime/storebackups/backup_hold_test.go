package storebackups

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ---- Test fakes ----

// fakeBackupStore implements the subset of store.BackupStore that BackupHold
// uses (ListAllBackupRepos, ListSucceededRecordsByRepo). All other methods
// panic — calling them is a test-authoring bug.
type fakeBackupStore struct {
	store.BackupStore // embed to inherit method set; unset methods panic via nil deref

	repos           []*models.BackupRepo
	recordsByRepoID map[string][]*models.BackupRecord

	listReposErr   error
	listRecordsErr map[string]error // keyed by repoID
}

func (f *fakeBackupStore) ListAllBackupRepos(_ context.Context) ([]*models.BackupRepo, error) {
	if f.listReposErr != nil {
		return nil, f.listReposErr
	}
	return f.repos, nil
}

func (f *fakeBackupStore) ListSucceededRecordsByRepo(_ context.Context, repoID string) ([]*models.BackupRecord, error) {
	if err, ok := f.listRecordsErr[repoID]; ok && err != nil {
		return nil, err
	}
	return f.recordsByRepoID[repoID], nil
}

// fakeHoldDestination implements destination.Destination with programmable
// GetManifestOnly behavior keyed by record ID. All non-GetManifestOnly / Close
// methods return errors because BackupHold never calls them.
type fakeHoldDestination struct {
	manifestsByID   map[string]*manifest.Manifest
	manifestErrByID map[string]error
	closeCalls      int
}

func (d *fakeHoldDestination) PutBackup(_ context.Context, _ *manifest.Manifest, _ io.Reader) error {
	return errors.New("PutBackup not implemented in fake")
}

func (d *fakeHoldDestination) GetManifestOnly(_ context.Context, id string) (*manifest.Manifest, error) {
	if err, ok := d.manifestErrByID[id]; ok && err != nil {
		return nil, err
	}
	m, ok := d.manifestsByID[id]
	if !ok {
		return nil, errors.New("manifest not found: " + id)
	}
	return m, nil
}

func (d *fakeHoldDestination) GetBackup(_ context.Context, _ string) (*manifest.Manifest, io.ReadCloser, error) {
	return nil, nil, errors.New("GetBackup not implemented in fake")
}

func (d *fakeHoldDestination) List(_ context.Context) ([]destination.BackupDescriptor, error) {
	return nil, errors.New("List not implemented in fake")
}

func (d *fakeHoldDestination) Stat(_ context.Context, _ string) (*destination.BackupDescriptor, error) {
	return nil, errors.New("Stat not implemented in fake")
}

func (d *fakeHoldDestination) Delete(_ context.Context, _ string) error {
	return errors.New("Delete not implemented in fake")
}

func (d *fakeHoldDestination) ValidateConfig(_ context.Context) error {
	return errors.New("ValidateConfig not implemented in fake")
}

func (d *fakeHoldDestination) Close() error {
	d.closeCalls++
	return nil
}

var _ destination.Destination = (*fakeHoldDestination)(nil)

// ---- Helpers ----

func mkManifest(id string, payloadIDs ...string) *manifest.Manifest {
	ids := payloadIDs
	if ids == nil {
		ids = []string{}
	}
	return &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        id,
		StoreID:         "store-test",
		StoreKind:       "memory",
		SHA256:          "deadbeef",
		PayloadIDSet:    ids,
	}
}

func mkRepo(id string) *models.BackupRepo {
	return &models.BackupRepo{ID: id, Kind: models.BackupRepoKindLocal}
}

func mkRecord(id, repoID string) *models.BackupRecord {
	return &models.BackupRecord{ID: id, RepoID: repoID, Status: models.BackupStatusSucceeded}
}

// ---- Tests ----

// TestBackupHold_UnionAcrossRepos verifies that PayloadIDSet fields from every
// succeeded record across every repo are unioned into the returned set.
func TestBackupHold_UnionAcrossRepos(t *testing.T) {
	ctx := context.Background()

	repoA := mkRepo("repo-a")
	repoB := mkRepo("repo-b")

	// Two repos, two records each, distinct + overlapping PayloadIDs.
	recA1 := mkRecord("rec-a1", repoA.ID)
	recA2 := mkRecord("rec-a2", repoA.ID)
	recB1 := mkRecord("rec-b1", repoB.ID)
	recB2 := mkRecord("rec-b2", repoB.ID)

	dst := &fakeHoldDestination{
		manifestsByID: map[string]*manifest.Manifest{
			"rec-a1": mkManifest("rec-a1", "payload-1", "payload-2"),
			"rec-a2": mkManifest("rec-a2", "payload-3"),
			"rec-b1": mkManifest("rec-b1", "payload-3", "payload-4"), // payload-3 overlaps with repoA
			"rec-b2": mkManifest("rec-b2", "payload-5"),
		},
	}

	bs := &fakeBackupStore{
		repos: []*models.BackupRepo{repoA, repoB},
		recordsByRepoID: map[string][]*models.BackupRecord{
			repoA.ID: {recA1, recA2},
			repoB.ID: {recB1, recB2},
		},
	}

	destFactory := func(_ context.Context, _ *models.BackupRepo) (destination.Destination, error) {
		return dst, nil
	}

	hold := NewBackupHold(bs, destFactory)
	got, err := hold.HeldPayloadIDs(ctx)
	if err != nil {
		t.Fatalf("HeldPayloadIDs: %v", err)
	}

	want := map[metadata.PayloadID]struct{}{
		"payload-1": {},
		"payload-2": {},
		"payload-3": {},
		"payload-4": {},
		"payload-5": {},
	}
	if len(got) != len(want) {
		t.Fatalf("union size: got %d, want %d — got=%v", len(got), len(want), got)
	}
	for pid := range want {
		if _, ok := got[pid]; !ok {
			t.Errorf("missing payloadID %q in union", pid)
		}
	}

	// Close must have been called once per repo (2 repos -> 2 calls; same dst
	// is reused by the factory, so the counter accumulates).
	if dst.closeCalls != 2 {
		t.Errorf("Close calls: got %d, want 2", dst.closeCalls)
	}
}

// TestBackupHold_ListReposFails verifies that a ListAllBackupRepos error is
// returned to the caller (infrastructure-level; not a continue-on-error case).
func TestBackupHold_ListReposFails(t *testing.T) {
	ctx := context.Background()

	sentinel := errors.New("db unavailable")
	bs := &fakeBackupStore{listReposErr: sentinel}

	destFactory := func(_ context.Context, _ *models.BackupRepo) (destination.Destination, error) {
		t.Fatal("destFactory must not be called when ListAllBackupRepos fails")
		return nil, nil
	}

	hold := NewBackupHold(bs, destFactory)
	got, err := hold.HeldPayloadIDs(ctx)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected error wrapping %v, got %v", sentinel, err)
	}
	if got != nil {
		t.Errorf("expected nil map on error, got %v", got)
	}
}

// TestBackupHold_DestFactoryFails_SkipsRepo verifies that destFactory failure
// on one repo logs WARN and skips the repo; other repos still contribute.
func TestBackupHold_DestFactoryFails_SkipsRepo(t *testing.T) {
	ctx := context.Background()

	repoA := mkRepo("repo-a")
	repoB := mkRepo("repo-b")

	recB := mkRecord("rec-b", repoB.ID)

	dstB := &fakeHoldDestination{
		manifestsByID: map[string]*manifest.Manifest{
			"rec-b": mkManifest("rec-b", "payload-b"),
		},
	}

	bs := &fakeBackupStore{
		repos: []*models.BackupRepo{repoA, repoB},
		recordsByRepoID: map[string][]*models.BackupRecord{
			repoB.ID: {recB},
		},
	}

	destFactory := func(_ context.Context, repo *models.BackupRepo) (destination.Destination, error) {
		if repo.ID == repoA.ID {
			return nil, errors.New("destFactory boom")
		}
		return dstB, nil
	}

	hold := NewBackupHold(bs, destFactory)
	got, err := hold.HeldPayloadIDs(ctx)
	if err != nil {
		t.Fatalf("HeldPayloadIDs: %v", err)
	}

	// Repo B still contributed — repoA was skipped.
	if _, ok := got[metadata.PayloadID("payload-b")]; !ok {
		t.Error("repoB payload missing; expected survivorship after repoA skip")
	}
	if len(got) != 1 {
		t.Errorf("unexpected extra payloads: got %d items, want 1 (%v)", len(got), got)
	}
	// dstB's Close called once.
	if dstB.closeCalls != 1 {
		t.Errorf("dstB.Close calls: got %d, want 1", dstB.closeCalls)
	}
}

// TestBackupHold_GetManifestOnlyFails_SkipsRecord verifies per-record errors
// are skipped (continue-on-error); other records still contribute.
func TestBackupHold_GetManifestOnlyFails_SkipsRecord(t *testing.T) {
	ctx := context.Background()

	repoA := mkRepo("repo-a")
	recGood := mkRecord("rec-good", repoA.ID)
	recBad := mkRecord("rec-bad", repoA.ID)

	dst := &fakeHoldDestination{
		manifestsByID: map[string]*manifest.Manifest{
			"rec-good": mkManifest("rec-good", "payload-good-1", "payload-good-2"),
			// rec-bad is absent from manifestsByID; manifestErrByID is explicit.
		},
		manifestErrByID: map[string]error{
			"rec-bad": errors.New("manifest corrupted"),
		},
	}

	bs := &fakeBackupStore{
		repos: []*models.BackupRepo{repoA},
		recordsByRepoID: map[string][]*models.BackupRecord{
			repoA.ID: {recGood, recBad},
		},
	}

	destFactory := func(_ context.Context, _ *models.BackupRepo) (destination.Destination, error) {
		return dst, nil
	}

	hold := NewBackupHold(bs, destFactory)
	got, err := hold.HeldPayloadIDs(ctx)
	if err != nil {
		t.Fatalf("HeldPayloadIDs: %v", err)
	}

	// Good record's payloads present; bad record's (none to present) omitted.
	for _, pid := range []metadata.PayloadID{"payload-good-1", "payload-good-2"} {
		if _, ok := got[pid]; !ok {
			t.Errorf("expected %q in union", pid)
		}
	}
	if len(got) != 2 {
		t.Errorf("unexpected size: got %d, want 2 (%v)", len(got), got)
	}
}

// TestBackupHold_EmptyWhenNoSucceededRecords verifies that an empty result set
// is returned (non-nil, len==0) when no records exist anywhere.
func TestBackupHold_EmptyWhenNoSucceededRecords(t *testing.T) {
	ctx := context.Background()

	repoA := mkRepo("repo-a")

	dst := &fakeHoldDestination{manifestsByID: map[string]*manifest.Manifest{}}
	bs := &fakeBackupStore{
		repos: []*models.BackupRepo{repoA},
		recordsByRepoID: map[string][]*models.BackupRecord{
			repoA.ID: nil, // no succeeded records
		},
	}

	destFactory := func(_ context.Context, _ *models.BackupRepo) (destination.Destination, error) {
		return dst, nil
	}

	hold := NewBackupHold(bs, destFactory)
	got, err := hold.HeldPayloadIDs(ctx)
	if err != nil {
		t.Fatalf("HeldPayloadIDs: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}
