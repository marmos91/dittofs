package shares

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestAddShare_RejectsColonInName guards the handle-encoding invariant: file
// handles are "<shareName>:<uuid>" split on the first ':'. A share name
// containing ':' produces handles whose UUID component fails to parse,
// silently bricking every file in the share. AddShare must reject such names
// with an ErrInvalidArgument StoreError before any state is created, while a
// normal name still succeeds and its handles round-trip.
func TestAddShare_RejectsColonInName(t *testing.T) {
	ctx := context.Background()

	t.Run("rejects colon-bearing name", func(t *testing.T) {
		mds := metamem.NewMemoryMetadataStoreWithDefaults()
		t.Cleanup(func() { _ = mds.Close() })

		svc := New()
		cfg := &ShareConfig{
			Name:          "/foo:bar",
			MetadataStore: "meta-test",
			Enabled:       true,
		}

		err := svc.AddShare(
			ctx,
			cfg,
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			nil,
			nil,
			nil,
		)
		if err == nil {
			t.Fatal("AddShare with ':' in name: want error, got nil")
		}
		var storeErr *metadata.StoreError
		if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrInvalidArgument {
			t.Fatalf("AddShare with ':' in name: want ErrInvalidArgument StoreError, got %v", err)
		}

		// Share must not have been registered.
		if _, gerr := svc.GetShare("/foo:bar"); gerr == nil {
			t.Fatal("colon-bearing share was registered despite rejection")
		}
	})

	t.Run("accepts normal name and handle round-trips", func(t *testing.T) {
		mds := metamem.NewMemoryMetadataStoreWithDefaults()
		t.Cleanup(func() { _ = mds.Close() })

		svc := New()
		const name = "/normal"
		cfg := &ShareConfig{
			Name:          name,
			MetadataStore: "meta-test",
			Enabled:       true,
		}

		if err := svc.AddShare(
			ctx,
			cfg,
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			nil,
			nil,
			nil,
		); err != nil {
			t.Fatalf("AddShare(%q): %v", name, err)
		}

		if _, err := svc.GetShare(name); err != nil {
			t.Fatalf("GetShare(%q): %v", name, err)
		}

		// A handle minted for this share must decode back to the same name.
		handle, err := metadata.GenerateNewHandle(name)
		if err != nil {
			t.Fatalf("GenerateNewHandle: %v", err)
		}
		gotName, _, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			t.Fatalf("DecodeFileHandle: %v", err)
		}
		if gotName != name {
			t.Fatalf("DecodeFileHandle share = %q, want %q", gotName, name)
		}
	})
}
