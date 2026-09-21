package shares

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// gatingRegistrar parks AddShare inside its metadata-registration phase — past
// the name reservation and past the block store it has already composed, but
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
// A rebind's pre-validation is that lookup, so arming it just before a rebind
// stops the rebind with its old store still live — which is where the fence then
// falls.
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
// a share arriving after shutdown has quiesced the data plane must not compose
// one. Its dispatcher would commit manifest rows through a metadata store the
// same shutdown is closing, which is exactly what the fence exists to stop.
func TestAddShare_RefusedOnceTheBlockStoreFenceHasRun(t *testing.T) {
	svc := New()
	store := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = store.Close() })
	defaults := journalDefaults(t, svc)

	svc.CloseBlockStores(context.Background())

	err := svc.AddShare(context.Background(),
		&ShareConfig{Name: "/late", MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID},
		fixedStoreProvider{store: store}, metadata.New(), memBlockStoreProvider{}, defaults, nil)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("AddShare after CloseBlockStores = %v, want ErrShuttingDown", err)
	}
	if _, err := svc.GetBlockStoreForShare("/late"); err == nil {
		t.Error("the refused share is registered with a running block store")
	}
}

// TestAddShare_RefusedWhenTheFenceFallsMidAdd is the half that matters. An
// AddShare already past its name reservation has composed and STARTED a block
// store, so the fence's registry snapshot does not contain it — refusing on
// entry alone would let this one publish a running dispatcher after shutdown
// had closed every other share's.
func TestAddShare_RefusedWhenTheFenceFallsMidAdd(t *testing.T) {
	svc := New()
	store := metamem.NewMemoryMetadataStoreWithDefaults()
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
			fixedStoreProvider{store: store}, gate, memBlockStoreProvider{}, defaults, nil)
	}()

	select {
	case <-gate.entered:
	case err := <-added:
		t.Fatalf("AddShare returned before reaching its metadata phase: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("AddShare never reached its metadata phase")
	}

	// The share holds a reservation and a started block store, and is not in
	// the registry — so this snapshot cannot see it.
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

	if _, err := svc.GetBlockStoreForShare("/midadd"); err == nil {
		t.Error("a share published a running block store after the shutdown fence closed every other one")
	}
}

// TestRebindShareBlockStore_RefusedWhenTheFenceFallsMidRebind is the back door
// the entry check cannot close. A rebind tears the live store down and builds a
// replacement; if the fence falls in between it closes the OLD store — an
// idempotent no-op, since the rebind already did — and the rebind then installs
// a fresh, running one behind it.
func TestRebindShareBlockStore_RefusedWhenTheFenceFallsMidRebind(t *testing.T) {
	const name = "/rebound"

	svc := New()
	store := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = store.Close() })
	defaults := journalDefaults(t, svc)
	provider := &gatingBlockStoreProvider{entered: make(chan struct{}), release: make(chan struct{})}

	cfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID}
	if err := svc.AddShare(context.Background(), cfg, fixedStoreProvider{store: store},
		metadata.New(), provider, defaults, nil); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	original, err := svc.GetBlockStoreForShare(name)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}

	newCfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true, BlockStoreID: "other-block-store"}
	provider.armed.Store(true)

	rebound := make(chan error, 1)
	go func() {
		rebound <- svc.RebindShareBlockStore(context.Background(), newCfg, cfg,
			fixedStoreProvider{store: store}, provider, defaults, nil)
	}()

	select {
	case <-provider.entered:
	case err := <-rebound:
		t.Fatalf("the rebind returned before resolving its new binding: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the rebind never resolved its new binding")
	}

	// The rebind is past its entry check with the old store still live, so the
	// fence sees and closes exactly the store the rebind is about to replace.
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

	current, err := svc.GetBlockStoreForShare(name)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare after the refused rebind: %v", err)
	}
	if current != original {
		t.Error("the rebind installed a fresh block store after the shutdown fence had closed the one it replaced")
	}
}
