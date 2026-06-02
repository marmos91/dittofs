package shares

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fixedStoreProvider returns one specific metadata store for every lookup. Two
// racing AddShare calls each get their OWN provider+store so we can detect a
// metadata-store/registry mismatch: if the loser's store ends up registered
// while the winner's share is in the registry, the two layers disagree.
type fixedStoreProvider struct{ store metadata.Store }

func (p fixedStoreProvider) GetMetadataStore(string) (metadata.Store, error) {
	return p.store, nil
}

// TestAddShare_ConcurrentSameName_NoMetadataMismatch is the REVIEW M2
// regression. Two goroutines race AddShare for the SAME name, each carrying a
// DISTINCT metadata store, against a shared real MetadataService. Exactly one
// must win; afterward the MetadataService's registered store for the name must
// be the SAME store the winning share exposes, and the loser must leave NO
// leaked metadata registration. Under the old ordering the loser's
// RegisterStoreForShare could win the last-writer-wins race on the metadata
// store map while losing the registry recheck, leaving the two layers pointing
// at different stores.
func TestAddShare_ConcurrentSameName_NoMetadataMismatch(t *testing.T) {
	const name = "/raced"

	for iter := 0; iter < 200; iter++ {
		ctx := context.Background()

		// Each contender gets its own distinct metadata store so a mismatch is
		// observable by pointer identity.
		storeA := metamem.NewMemoryMetadataStoreWithDefaults()
		storeB := metamem.NewMemoryMetadataStoreWithDefaults()

		svc := New()
		metaSvc := metadata.New()

		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		var winnerStore metadata.Store

		run := func(store metadata.Store) {
			defer wg.Done()
			cfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true}
			err := svc.AddShare(ctx, cfg, fixedStoreProvider{store: store}, metaSvc, nil, nil, nil)
			if err == nil {
				mu.Lock()
				winners++
				winnerStore = store
				mu.Unlock()
			}
		}

		wg.Add(2)
		go run(storeA)
		go run(storeB)
		wg.Wait()

		if winners != 1 {
			t.Fatalf("iter %d: expected exactly one AddShare to win, got %d", iter, winners)
		}

		// The registry must expose the winner's share.
		if _, err := svc.GetShare(name); err != nil {
			t.Fatalf("iter %d: winner share not in registry: %v", iter, err)
		}

		// INVARIANT: the MetadataService's registered store for the name must be
		// the SAME store the winning AddShare used — no loser-store leak.
		got, err := metaSvc.GetStoreForShare(name)
		if err != nil {
			t.Fatalf("iter %d: MetadataService has no store for winner share: %v", iter, err)
		}
		if got != winnerStore {
			t.Fatalf("iter %d: metadata/registry MISMATCH: MetadataService store != winner's store", iter)
		}

		_ = storeA.Close()
		_ = storeB.Close()
	}
}

// TestAddShare_ConcurrentSameName_SecondCallerRejected pins the simpler
// invariant that a second AddShare for a name already (or concurrently) added
// is rejected, and the first caller's share survives intact.
func TestAddShare_ConcurrentSameName_SecondCallerRejected(t *testing.T) {
	const name = "/once"
	ctx := context.Background()

	store := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = store.Close() })

	svc := New()
	metaSvc := metadata.New()

	cfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true}
	if err := svc.AddShare(ctx, cfg, fixedStoreProvider{store: store}, metaSvc, nil, nil, nil); err != nil {
		t.Fatalf("first AddShare: %v", err)
	}

	// Second add for the same name must fail.
	if err := svc.AddShare(ctx, cfg, fixedStoreProvider{store: store}, metaSvc, nil, nil, nil); err == nil {
		t.Fatal("second AddShare for existing name: want error, got nil")
	}

	// The share must still resolve in both layers.
	if _, err := svc.GetShare(name); err != nil {
		t.Fatalf("share missing after rejected re-add: %v", err)
	}
	if _, err := metaSvc.GetStoreForShare(name); err != nil {
		t.Fatalf("metadata store missing after rejected re-add: %v", err)
	}
}
