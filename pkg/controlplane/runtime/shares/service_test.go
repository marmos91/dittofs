package shares

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// fakeShareStore is a minimal in-memory ShareStore sufficient for exercising
// DisableShare / EnableShare. Only GetShare and UpdateShare are used.
type fakeShareStore struct {
	mu          sync.Mutex
	shares      map[string]*models.Share // name -> share
	updateErr   error                    // injected error for UpdateShare
	updateCount int                      // count of UpdateShare calls
}

func newFakeShareStore() *fakeShareStore {
	return &fakeShareStore{shares: make(map[string]*models.Share)}
}

func (f *fakeShareStore) seed(share *models.Share) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy to decouple from caller.
	cp := *share
	f.shares[share.Name] = &cp
}

func (f *fakeShareStore) GetShare(ctx context.Context, name string) (*models.Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shares[name]
	if !ok {
		return nil, models.ErrShareNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeShareStore) UpdateShare(ctx context.Context, share *models.Share) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCount++
	if f.updateErr != nil {
		return f.updateErr
	}
	if _, ok := f.shares[share.Name]; !ok {
		return models.ErrShareNotFound
	}
	cp := *share
	f.shares[share.Name] = &cp
	return nil
}

// countUpdates returns the observed UpdateShare call count.
func (f *fakeShareStore) countUpdates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateCount
}

// makeService creates a Service with the given shares pre-registered, and a
// fake store seeded with matching DB rows. Each share's runtime Enabled
// mirrors the DB-row Enabled so the test starts in a consistent state.
func makeService(t *testing.T, shares ...*Share) (*Service, *fakeShareStore) {
	t.Helper()
	svc := New()
	store := newFakeShareStore()
	for _, sh := range shares {
		svc.InjectShareForTesting(sh)
		store.seed(&models.Share{
			ID:              fmt.Sprintf("id-%s", sh.Name),
			Name:            sh.Name,
			MetadataStoreID: "ms-" + sh.MetadataStore,
			Enabled:         sh.Enabled,
		})
	}
	return svc, store
}

func TestDisableShare_HappyPath(t *testing.T) {
	svc, store := makeService(t, &Share{
		Name:          "/export",
		MetadataStore: "meta-a",
		Enabled:       true,
	})

	// Observe notify callback fire.
	var notified atomic.Int32
	svc.OnShareChange(func(_ []string) { notified.Add(1) })

	if err := svc.DisableShare(context.Background(), store, "/export"); err != nil {
		t.Fatalf("DisableShare: %v", err)
	}

	// DB state
	dbShare, err := store.GetShare(context.Background(), "/export")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if dbShare.Enabled {
		t.Error("expected DB Enabled=false after DisableShare")
	}

	// Runtime state
	got, err := svc.IsShareEnabled("/export")
	if err != nil {
		t.Fatalf("IsShareEnabled: %v", err)
	}
	if got {
		t.Error("expected runtime Enabled=false after DisableShare")
	}

	if n := notified.Load(); n != 1 {
		t.Errorf("expected notifyShareChange fired exactly once, got %d", n)
	}
}

func TestDisableShare_AlreadyDisabled(t *testing.T) {
	svc, store := makeService(t, &Share{
		Name:          "/export",
		MetadataStore: "meta-a",
		Enabled:       false,
	})
	// Seed DB to already-disabled state so DisableShare observes the idempotent path.
	store.seed(&models.Share{Name: "/export", Enabled: false})

	preUpdateCount := store.countUpdates()
	err := svc.DisableShare(context.Background(), store, "/export")
	if !errors.Is(err, ErrShareAlreadyDisabled) {
		t.Fatalf("expected ErrShareAlreadyDisabled, got %v", err)
	}
	if got := store.countUpdates(); got != preUpdateCount {
		t.Errorf("expected no UpdateShare call on already-disabled (idempotent), got %d additional", got-preUpdateCount)
	}
}

func TestDisableShare_DBWriteFails_RuntimeUnchanged(t *testing.T) {
	svc, store := makeService(t, &Share{
		Name:          "/export",
		MetadataStore: "meta-a",
		Enabled:       true,
	})
	injected := errors.New("simulated DB failure")
	store.updateErr = injected

	err := svc.DisableShare(context.Background(), store, "/export")
	if err == nil {
		t.Fatal("expected error when DB write fails")
	}
	if !errors.Is(err, injected) {
		t.Errorf("expected wrapped DB error, got %v", err)
	}

	// Runtime must stay enabled (DB-first-then-runtime rule).
	got, err := svc.IsShareEnabled("/export")
	if err != nil {
		t.Fatalf("IsShareEnabled: %v", err)
	}
	if !got {
		t.Error("expected runtime Enabled=true to survive DB-write failure")
	}
}

func TestSetShareNetgroup(t *testing.T) {
	svc, _ := makeService(t, &Share{
		Name:          "/export",
		MetadataStore: "meta-a",
		Enabled:       true,
	})

	if err := svc.SetShareNetgroup("/export", "office"); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}
	sh, err := svc.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if sh.NetgroupName != "office" {
		t.Errorf("NetgroupName = %q, want office", sh.NetgroupName)
	}

	// Empty name clears the association.
	if err := svc.SetShareNetgroup("/export", ""); err != nil {
		t.Fatalf("SetShareNetgroup(clear): %v", err)
	}
	sh, _ = svc.GetShare("/export")
	if sh.NetgroupName != "" {
		t.Errorf("NetgroupName = %q, want empty after clear", sh.NetgroupName)
	}
}

func TestSetShareNetgroup_UnknownShare(t *testing.T) {
	svc, _ := makeService(t)

	err := svc.SetShareNetgroup("/missing", "office")
	if !errors.Is(err, ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}
}

func TestEnableShare_Idempotent(t *testing.T) {
	svc, store := makeService(t, &Share{
		Name:          "/export",
		MetadataStore: "meta-a",
		Enabled:       true,
	})
	preUpdateCount := store.countUpdates()

	if err := svc.EnableShare(context.Background(), store, "/export"); err != nil {
		t.Fatalf("EnableShare on already-enabled share: %v", err)
	}
	if got := store.countUpdates(); got != preUpdateCount {
		t.Errorf("expected no UpdateShare call on already-enabled share, got %d additional", got-preUpdateCount)
	}
}

func TestEnableShare_FlipsFromDisabled(t *testing.T) {
	svc, store := makeService(t, &Share{
		Name:          "/export",
		MetadataStore: "meta-a",
		Enabled:       false,
	})
	// Seed DB to disabled.
	store.seed(&models.Share{Name: "/export", Enabled: false})

	if err := svc.EnableShare(context.Background(), store, "/export"); err != nil {
		t.Fatalf("EnableShare: %v", err)
	}
	dbShare, _ := store.GetShare(context.Background(), "/export")
	if !dbShare.Enabled {
		t.Error("expected DB Enabled=true after EnableShare")
	}
	got, err := svc.IsShareEnabled("/export")
	if err != nil {
		t.Fatalf("IsShareEnabled: %v", err)
	}
	if !got {
		t.Error("expected runtime Enabled=true after EnableShare")
	}
}

func TestIsShareEnabled_UnknownShare(t *testing.T) {
	svc, _ := makeService(t)
	got, err := svc.IsShareEnabled("/nope")
	if got {
		t.Error("expected Enabled=false for unknown share")
	}
	if !errors.Is(err, ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}
}

func TestListEnabledSharesForStore_FiltersCorrectly(t *testing.T) {
	svc, _ := makeService(t,
		&Share{Name: "/a1", MetadataStore: "meta-a", Enabled: true},
		&Share{Name: "/a2", MetadataStore: "meta-a", Enabled: false},
		&Share{Name: "/b1", MetadataStore: "meta-b", Enabled: true},
	)

	got := svc.ListEnabledSharesForStore("meta-a")
	sort.Strings(got)
	want := []string{"/a1"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("expected %v, got %v", want, got)
	}

	gotB := svc.ListEnabledSharesForStore("meta-b")
	sort.Strings(gotB)
	if len(gotB) != 1 || gotB[0] != "/b1" {
		t.Errorf("expected [/b1] for meta-b, got %v", gotB)
	}

	// Store with no enabled shares -> empty slice (nil allowed).
	gotC := svc.ListEnabledSharesForStore("meta-c")
	if len(gotC) != 0 {
		t.Errorf("expected empty for meta-c, got %v", gotC)
	}
}

// TestTrashSettingsForShare_NoRace runs concurrent writers (SetShareTrashConfig)
// against concurrent readers (TrashSettingsForShare) to prove the locked
// accessor + setter never race (refs #190, #936). The writer mutates the exact
// fields the reader copies, so `go test -race` would flag an unlocked read.
func TestTrashSettingsForShare_NoRace(t *testing.T) {
	const name = "/export"
	svc, _ := makeService(t, &Share{
		Name:          name,
		MetadataStore: "meta-a",
		Enabled:       true,
		TrashEnabled:  true,
	})

	const iters = 5000
	var wg sync.WaitGroup

	// One writer goroutine mutating the exact fields the readers copy.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			// store=nil: exercise the runtime-only path so the test does not
			// depend on DB persistence semantics.
			_ = svc.SetShareTrashConfig(nil, name, TrashSettings{
				Enabled:         i%2 == 0,
				RetentionDays:   i,
				RestrictToAdmin: i%3 == 0,
				MaxBytes:        int64(i) * 1024,
				ExcludePatterns: []string{fmt.Sprintf("*.tmp%d", i)},
			})
		}
	}()

	// Several concurrent readers to maximize overlap with the writer; without
	// the RLock in TrashSettingsForShare, `go test -race` flags these.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, _ = svc.TrashSettingsForShare(name)
				_ = svc.EnabledTrashShares()
			}
		}()
	}
	wg.Wait()
}

// TestSetShareTrashConfig_RoundTrip verifies SetShareTrashConfig then
// TrashSettingsForShare returns the set values, including a deep-copied
// ExcludePatterns slice (mutating the returned slice must not affect state).
func TestSetShareTrashConfig_RoundTrip(t *testing.T) {
	const name = "/export"
	svc, store := makeService(t, &Share{
		Name:          name,
		MetadataStore: "meta-a",
		Enabled:       true,
	})

	want := TrashSettings{
		Enabled:         true,
		RetentionDays:   30,
		RestrictToAdmin: true,
		MaxBytes:        5 << 30,
		ExcludePatterns: []string{"*.tmp", "~$*"},
	}
	if err := svc.SetShareTrashConfig(store, name, want); err != nil {
		t.Fatalf("SetShareTrashConfig: %v", err)
	}

	got, ok := svc.TrashSettingsForShare(name)
	if !ok {
		t.Fatal("TrashSettingsForShare: share not found")
	}
	if got.Enabled != want.Enabled ||
		got.RetentionDays != want.RetentionDays ||
		got.RestrictToAdmin != want.RestrictToAdmin ||
		got.MaxBytes != want.MaxBytes {
		t.Errorf("scalar mismatch: got %+v want %+v", got, want)
	}
	if len(got.ExcludePatterns) != 2 || got.ExcludePatterns[0] != "*.tmp" || got.ExcludePatterns[1] != "~$*" {
		t.Errorf("ExcludePatterns mismatch: got %v", got.ExcludePatterns)
	}

	// Mutating the returned slice must not corrupt the share's stored slice.
	got.ExcludePatterns[0] = "MUTATED"
	again, _ := svc.TrashSettingsForShare(name)
	if again.ExcludePatterns[0] != "*.tmp" {
		t.Errorf("returned slice aliases internal state: %v", again.ExcludePatterns)
	}

	// DB persistence round-trip.
	dbShare, err := store.GetShare(context.Background(), name)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !dbShare.TrashEnabled || dbShare.TrashRetentionDays != 30 ||
		!dbShare.TrashRestrictToAdmin || dbShare.TrashMaxBytes != (5<<30) {
		t.Errorf("DB scalar mismatch: %+v", dbShare)
	}
	if pats := dbShare.GetTrashExcludePatterns(); len(pats) != 2 || pats[0] != "*.tmp" {
		t.Errorf("DB ExcludePatterns mismatch: %v", pats)
	}
}

// TestSetShareTrashConfig_UnknownShare verifies the setter returns
// ErrShareNotFound for an unregistered share.
func TestSetShareTrashConfig_UnknownShare(t *testing.T) {
	svc := New()
	err := svc.SetShareTrashConfig(nil, "/nope", TrashSettings{Enabled: true})
	if !errors.Is(err, ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}
}

// TestEnabledTrashShares_FiltersByFlag verifies only trash-enabled shares are
// returned.
// TestGetShare_NoRace verifies GetShare hands out a snapshot, not the live
// registry pointer: concurrent UpdateShare writers mutate the exact scalar
// fields readers inspect off the returned *Share. Without the under-lock copy
// in GetShare, `go test -race` flags the read of share.ReadOnly /
// DefaultPermission against UpdateShare's write.
func TestGetShare_NoRace(t *testing.T) {
	const name = "/export"
	svc, _ := makeService(t, &Share{
		Name:              name,
		MetadataStore:     "meta-a",
		Enabled:           true,
		ReadOnly:          false,
		DefaultPermission: "read",
	})

	const iters = 5000
	var wg sync.WaitGroup

	// Writer mutating the exact fields readers copy out of GetShare.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ro := i%2 == 0
			perm := "read"
			if i%3 == 0 {
				perm = "read-write"
			}
			_ = svc.UpdateShare(name, &ro, &perm, nil, nil)
		}
	}()

	// Concurrent readers reading the snapshot's scalar fields.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				sh, err := svc.GetShare(name)
				if err != nil {
					t.Errorf("GetShare: %v", err)
					return
				}
				_ = sh.ReadOnly
				_ = sh.DefaultPermission
				_ = sh.Enabled
			}
		}()
	}
	wg.Wait()
}

// TestGetShare_ReturnsSnapshotNotLivePointer verifies the returned *Share is a
// copy: mutating it must not change registry state observed by a later
// GetShare, and a later UpdateShare must not retroactively mutate an
// already-returned snapshot.
func TestGetShare_ReturnsSnapshotNotLivePointer(t *testing.T) {
	const name = "/export"
	svc, _ := makeService(t, &Share{
		Name:          name,
		MetadataStore: "meta-a",
		Enabled:       true,
		ReadOnly:      false,
	})

	first, err := svc.GetShare(name)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	// Mutating the snapshot must not leak into the registry.
	first.ReadOnly = true
	second, err := svc.GetShare(name)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if second.ReadOnly {
		t.Error("mutating a GetShare result leaked into registry state")
	}

	// A subsequent UpdateShare must not mutate the already-returned snapshot.
	ro := true
	if err := svc.UpdateShare(name, &ro, nil, nil, nil); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	if second.ReadOnly {
		t.Error("UpdateShare retroactively mutated a previously returned snapshot")
	}
}

func TestEnabledTrashShares_FiltersByFlag(t *testing.T) {
	svc, _ := makeService(t,
		&Share{Name: "/on", MetadataStore: "m", Enabled: true, TrashEnabled: true},
		&Share{Name: "/off", MetadataStore: "m", Enabled: true, TrashEnabled: false},
	)
	got := svc.EnabledTrashShares()
	if len(got) != 1 || got[0] != "/on" {
		t.Errorf("expected [/on], got %v", got)
	}
}
