package shares

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
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
			BlockStoreID:  testBlockStoreID,
		}

		err := svc.AddShare(
			ctx,
			cfg,
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			memBlockStoreProvider{},
			journalDefaults(t, svc),
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
			BlockStoreID:  testBlockStoreID,
		}

		if err := svc.AddShare(
			ctx,
			cfg,
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			memBlockStoreProvider{},
			journalDefaults(t, svc),
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

// TestAddShare_RejectsOverLongName guards the other half of the same
// handle-encoding invariant: handles are "<shareName>:<uuid>" capped at
// metadata.MaxFileHandleSize bytes, so a share name longer than
// metadata.MaxShareNameLen can never mint one. Such a share used to be
// accepted and then fail every file operation at handle mint — panicking the
// memory backend outright. AddShare must reject it up front.
func TestAddShare_RejectsOverLongName(t *testing.T) {
	ctx := context.Background()

	longest := "/" + strings.Repeat("a", metadata.MaxShareNameLen-1)
	tooLong := longest + "a"

	t.Run("rejects over-long name", func(t *testing.T) {
		mds := metamem.NewMemoryMetadataStoreWithDefaults()
		t.Cleanup(func() { _ = mds.Close() })

		svc := New()
		err := svc.AddShare(
			ctx,
			&ShareConfig{Name: tooLong, MetadataStore: "meta-test", Enabled: true, BlockStoreID: testBlockStoreID},
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			memBlockStoreProvider{},
			journalDefaults(t, svc),
			nil,
		)
		if err == nil {
			t.Fatal("AddShare with over-long name: want error, got nil")
		}
		var storeErr *metadata.StoreError
		if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrInvalidArgument {
			t.Fatalf("AddShare with over-long name: want ErrInvalidArgument StoreError, got %v", err)
		}
		// The message must tell the operator both the limit and the overage.
		wantLen := strconv.Itoa(len(tooLong))
		wantMax := strconv.Itoa(metadata.MaxShareNameLen)
		if !strings.Contains(err.Error(), wantLen) || !strings.Contains(err.Error(), wantMax) {
			t.Fatalf("error must state the offending length (%s) and the limit (%s), got %q",
				wantLen, wantMax, err.Error())
		}
		if _, gerr := svc.GetShare(tooLong); gerr == nil {
			t.Fatal("over-long share was registered despite rejection")
		}
	})

	t.Run("accepts the longest encodable name", func(t *testing.T) {
		mds := metamem.NewMemoryMetadataStoreWithDefaults()
		t.Cleanup(func() { _ = mds.Close() })

		svc := New()
		if err := svc.AddShare(
			ctx,
			&ShareConfig{Name: longest, MetadataStore: "meta-test", Enabled: true, BlockStoreID: testBlockStoreID},
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			memBlockStoreProvider{},
			journalDefaults(t, svc),
			nil,
		); err != nil {
			t.Fatalf("AddShare(%q) (%d bytes, the limit): %v", longest, len(longest), err)
		}
		if _, err := metadata.GenerateNewHandle(longest); err != nil {
			t.Fatalf("GenerateNewHandle(%q): %v", longest, err)
		}
	})
}

// newAddShareFixture wires a service to a fresh in-memory metadata store and
// returns it with its journal defaults and a closure that adds a share by name,
// so a test about how two spellings of one name behave states only the names.
func newAddShareFixture(t *testing.T) (*Service, *LocalStoreDefaults, func(name string) error) {
	t.Helper()

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	svc := New()
	defaults := journalDefaults(t, svc)
	add := func(name string) error {
		return svc.AddShare(
			context.Background(),
			&ShareConfig{Name: name, MetadataStore: "meta-test", Enabled: true, BlockStoreID: testBlockStoreID},
			&metaStoreProvider{name: "meta-test", store: mds},
			metaSvcRegistrar{},
			memBlockStoreProvider{},
			defaults,
			nil,
		)
	}
	return svc, defaults, add
}

// TestAddShare_LeadingSlashIsOneName guards the directory invariant
// OpenShareJournal relies on: two shares never share a journal directory.
// "alpha" and "/alpha" sanitize to the same directory, so they must not be two
// shares — the name is normalized to one spelling at the seam, which makes the
// second registration a duplicate instead of a second writer into one journal.
func TestAddShare_LeadingSlashIsOneName(t *testing.T) {
	svc, defaults, add := newAddShareFixture(t)

	// The premise: both spellings address one directory.
	if unslashed, slashed := ShareJournalDir(defaults.JournalRoot, "alpha"), ShareJournalDir(defaults.JournalRoot, "/alpha"); unslashed != slashed {
		t.Fatalf("expected both spellings to resolve to one directory, got %q and %q", unslashed, slashed)
	}

	if err := add("alpha"); err != nil {
		t.Fatalf(`AddShare("alpha"): %v`, err)
	}
	if _, err := svc.GetShare("/alpha"); err != nil {
		t.Fatalf(`a share added as "alpha" must be registered as "/alpha": %v`, err)
	}

	err := add("/alpha")
	if err == nil {
		t.Fatal(`AddShare("/alpha") was accepted alongside "alpha": both write into one journal directory`)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf(`AddShare("/alpha") must be refused as a duplicate, got %v`, err)
	}
}

// TestAddShare_PercentInNameIsALiteral guards the same directory invariant for
// a name a control-plane row can actually hold: "a%2Fb". The fold must leave
// the percent sign alone, so both spellings of that name open the directory the
// name itself sanitizes to and not the one its decoded reading would — a share
// registered under the decoded name writes into a directory no row addresses,
// and the unfolded spelling would then open a second journal beside it.
func TestAddShare_PercentInNameIsALiteral(t *testing.T) {
	// The name as a control-plane row holds it, and the different name that
	// reading its percent sign as an escape would produce.
	const persisted = "a%2Fb"
	const decoded = "a/b"

	svc, defaults, add := newAddShareFixture(t)

	literalDir := ShareJournalDir(defaults.JournalRoot, persisted)
	decodedDir := ShareJournalDir(defaults.JournalRoot, decoded)

	// The premise: both spellings of the persisted name address one directory,
	// and it is not the one the decoded reading addresses.
	if slashed := ShareJournalDir(defaults.JournalRoot, "/"+persisted); literalDir != slashed {
		t.Fatalf("expected both spellings of %q to resolve to one directory, got %q and %q", persisted, literalDir, slashed)
	}
	if literalDir == decodedDir {
		t.Fatalf("premise broken: %q and %q must address different directories, both gave %q", persisted, decoded, literalDir)
	}

	if err := add(persisted); err != nil {
		t.Fatalf("AddShare(%q): %v", persisted, err)
	}

	// The share opened the directory its own name sanitizes to, not the one the
	// decoded reading would have picked.
	if _, err := os.Stat(literalDir); err != nil {
		t.Fatalf("share %q did not open its own journal directory %q: %v", persisted, literalDir, err)
	}
	if _, err := os.Stat(decodedDir); err == nil {
		t.Fatalf("share %q opened the journal directory %q of its decoded reading %q", persisted, decodedDir, decoded)
	}

	// ...and it is registered under the name as written.
	if _, err := svc.GetShare("/" + persisted); err != nil {
		t.Fatalf("a share added as %q must be registered as %q: %v", persisted, "/"+persisted, err)
	}
	if _, err := svc.GetShare("/" + decoded); err == nil {
		t.Fatalf("share %q was registered under its decoded reading %q", persisted, "/"+decoded)
	}

	// The slashed spelling addresses that same directory, so it must be refused
	// as a duplicate rather than opening a second journal in it.
	err := add("/" + persisted)
	if err == nil {
		t.Fatalf("AddShare(%q) was accepted alongside %q: both write into %q", "/"+persisted, persisted, literalDir)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("AddShare(%q) must be refused as a duplicate, got %v", "/"+persisted, err)
	}
}
