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
