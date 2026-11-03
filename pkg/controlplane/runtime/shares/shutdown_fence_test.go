package shares

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// gatingRegistrar parks AddShare inside its metadata-registration phase — past
// the name reservation and the remote store it has already acquired, but
// before the share is published. That is the window the fence has to survive,
// and parking in it is what makes the test deterministic: nothing here waits on
// a duration to line two goroutines up.
type gatingRegistrar struct {
	*metadata.Service
	entered chan struct{}
	release chan struct{}
}

func (g *gatingRegistrar) RegisterStoreForShare(shareName string, store metadata.Store) error {
	close(g.entered)
	<-g.release
	return g.Service.RegisterStoreForShare(shareName, store)
}

// gatingBlockStoreProvider parks the first config lookup made after it is armed.
// A rebind resolves its new remote through that lookup, so arming it just
// before a rebind stops the rebind with its old binding still live — which is
// where the fence then falls.
type gatingBlockStoreProvider struct {
	memBlockStoreProvider
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatingBlockStoreProvider) gate() {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
}

func (g *gatingBlockStoreProvider) GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error) {
	g.gate()
	return g.memBlockStoreProvider.GetBlockStoreByID(ctx, id)
}

func (g *gatingBlockStoreProvider) GetBlockStore(ctx context.Context, name string) (*models.BlockStoreConfig, error) {
	g.gate()
	return g.memBlockStoreProvider.GetBlockStore(ctx, name)
}

// TestAddShare_RefusedOnceTheBlockStoreFenceHasRun is the straightforward half:
// a share arriving after shutdown has run the fence must not be published.
func TestAddShare_RefusedOnceTheBlockStoreFenceHasRun(t *testing.T) {
	svc := New()
	store := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = store.Close() })
	defaults := journalDefaults(t, svc)

	svc.CloseBlockStores(context.Background())

	err := svc.AddShare(context.Background(),
		&ShareConfig{Name: "/late", MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID},
		fixedStoreProvider{store: store}, metadata.New(), memBlockStoreProvider{}, defaults)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("AddShare after CloseBlockStores = %v, want ErrShuttingDown", err)
	}
	if svc.ShareExists("/late") {
		t.Error("the refused share is registered")
	}
}

// TestAddShare_RefusedWhenTheFenceFallsMidAdd is the half that matters. An
// AddShare already past its name reservation is not refused by the entry
// check, so only the check at publication keeps it from appearing after
// shutdown.
func TestAddShare_RefusedWhenTheFenceFallsMidAdd(t *testing.T) {
	svc := New()
	store := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = store.Close() })
	defaults := journalDefaults(t, svc)

	gate := &gatingRegistrar{
		Service: metadata.New(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	added := make(chan error, 1)
	go func() {
		added <- svc.AddShare(context.Background(),
			&ShareConfig{Name: "/midadd", MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID},
			fixedStoreProvider{store: store}, gate, memBlockStoreProvider{}, defaults)
	}()

	select {
	case <-gate.entered:
	case err := <-added:
		t.Fatalf("AddShare returned before reaching its metadata phase: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("AddShare never reached its metadata phase")
	}

	// The share holds a reservation and is not in the registry.
	svc.CloseBlockStores(context.Background())
	close(gate.release)

	select {
	case err := <-added:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("AddShare finishing after the fence = %v, want ErrShuttingDown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("AddShare never returned")
	}

	if svc.ShareExists("/midadd") {
		t.Error("a share was published after the shutdown fence")
	}
}

// TestRebindShareBlockStore_RefusedByTheFence covers both halves for a rebind:
// one arriving after the fence is refused on entry, and one the fence falls
// under, while it resolves its new remote, is refused at the swap and leaves
// the share on the remote it had.
func TestRebindShareBlockStore_RefusedByTheFence(t *testing.T) {
	const name = "/rebound"

	svc := New()
	store := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = store.Close() })
	provider := &gatingBlockStoreProvider{entered: make(chan struct{}), release: make(chan struct{})}

	cfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID}
	if err := svc.AddShare(context.Background(), cfg, fixedStoreProvider{store: store},
		metadata.New(), provider, journalDefaults(t, svc)); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	provider.armed.Store(true)
	rebound := make(chan error, 1)
	go func() {
		rebound <- svc.RebindShareBlockStore(context.Background(), name, "other-block-store", provider)
	}()

	select {
	case <-provider.entered:
	case err := <-rebound:
		t.Fatalf("the rebind returned before resolving its new binding: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the rebind never resolved its new binding")
	}
	svc.CloseBlockStores(context.Background())
	close(provider.release)

	select {
	case err := <-rebound:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("the rebind finishing after the fence = %v, want ErrShuttingDown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebind never returned")
	}
	if entries := svc.DistinctRemoteStores(); len(entries) != 1 || entries[0].ConfigID != testBlockStoreID {
		t.Errorf("bound remotes after the refused rebind = %+v, want only %s", entries, testBlockStoreID)
	}

	if err := svc.RebindShareBlockStore(context.Background(), name, "other-block-store", provider); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("a rebind after the fence = %v, want ErrShuttingDown", err)
	}
}
