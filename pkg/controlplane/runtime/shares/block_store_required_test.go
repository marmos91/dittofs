package shares

import (
	"context"
	"strings"
	"testing"

	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestAddShare_RequiresBlockStore pins the durability invariant: a share's
// block store is its only durable tier, so a share configured without one is
// refused rather than built. Without the refusal the share comes up with a
// journal alone, and eviction is free to reclaim journal space that holds the
// only copy of a block.
func TestAddShare_RequiresBlockStore(t *testing.T) {
	ctx := context.Background()

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	svc := New()
	const name = "/no-blocks"
	err := svc.AddShare(
		ctx,
		&ShareConfig{Name: name, MetadataStore: "meta-test", Enabled: true},
		&metaStoreProvider{name: "meta-test", store: mds},
		metaSvcRegistrar{},
		memBlockStoreProvider{},
		&LocalStoreDefaults{JournalRoot: t.TempDir()},
		nil,
	)
	if err == nil {
		t.Fatal("AddShare without a block store: want error, got nil")
	}
	if !strings.Contains(err.Error(), "block store") {
		t.Fatalf("error must name the missing block store, got %q", err.Error())
	}
	if _, gerr := svc.GetShare(name); gerr == nil {
		t.Fatal("share without a block store was registered despite the refusal")
	}
}
