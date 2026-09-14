package store

import (
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// oldShapeDBWithStores adds the block store configs the shares table used to
// point at, so a share's recorded data location can be reached the way the
// migration reaches it.
func oldShapeDBWithStores(t *testing.T) *gorm.DB {
	t.Helper()
	db := oldShapeDB(t)
	if err := db.Exec(`CREATE TABLE block_store_configs (
		id TEXT,
		name TEXT,
		config TEXT
	)`).Error; err != nil {
		t.Fatalf("create block_store_configs: %v", err)
	}
	return db
}

func insertBlockStore(t *testing.T, db *gorm.DB, id, name, config string) {
	t.Helper()
	if err := db.Exec(
		"INSERT INTO block_store_configs (id, name, config) VALUES (?, ?, ?)",
		id, name, config,
	).Error; err != nil {
		t.Fatalf("insert block store %s: %v", id, err)
	}
}

// TestCheckShareJournalRoots_RefusesAShareRecordedElsewhere is the point of the
// guard: the share's bytes are under /srv/dittofs, the configured root names
// somewhere else, and dropping the column would leave nothing to notice with.
func TestCheckShareJournalRoots_RefusesAShareRecordedElsewhere(t *testing.T) {
	db := oldShapeDBWithStores(t)
	insertBlockStore(t, db, "local-id", "local-fs", `{"path":"/srv/dittofs"}`)
	insertShare(t, db, "/archive", "remote-id", "local-id")

	err := checkShareJournalRoots(db, "/var/lib/dittofs/blocks")
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("checkShareJournalRoots = %v, want ErrJournalRootMismatch", err)
	}
	if !strings.Contains(err.Error(), "/archive") {
		t.Errorf("the refusal must name the share an operator has to fix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/srv/dittofs") {
		t.Errorf("the refusal must name where the data actually is, got: %v", err)
	}
}

// The reference historically held the block store's name instead of its UUID,
// so a share recorded that way must still be reached.
func TestCheckShareJournalRoots_ReachesAStoreReferencedByName(t *testing.T) {
	db := oldShapeDBWithStores(t)
	insertBlockStore(t, db, "local-id", "local-fs", `{"path":"/srv/dittofs"}`)
	insertShare(t, db, "/archive", "remote-id", "local-fs")

	if err := checkShareJournalRoots(db, "/var/lib/dittofs/blocks"); !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("a store referenced by name must still be compared, got %v", err)
	}
}

func TestCheckShareJournalRoots_PassesWhenThePathIsTheConfiguredRoot(t *testing.T) {
	db := oldShapeDBWithStores(t)
	insertBlockStore(t, db, "local-id", "local-fs", `{"path":"/var/lib/dittofs/blocks"}`)
	insertShare(t, db, "/archive", "remote-id", "local-id")

	if err := checkShareJournalRoots(db, "/var/lib/dittofs/blocks/"); err != nil {
		t.Fatalf("a share already under the configured root must pass, got %v", err)
	}
}

// Every embedder that never configured a journal reaches the upgrade with no
// root at all; there is nothing to compare against, so nothing is refused.
func TestCheckShareJournalRoots_SkipsWhenNoRootIsConfigured(t *testing.T) {
	db := oldShapeDBWithStores(t)
	insertBlockStore(t, db, "local-id", "local-fs", `{"path":"/srv/dittofs"}`)
	insertShare(t, db, "/archive", "remote-id", "local-id")

	if err := checkShareJournalRoots(db, ""); err != nil {
		t.Fatalf("no configured root means no comparison, got %v", err)
	}
}

// A memory store kept no directory, so it records no location to disagree with.
func TestCheckShareJournalRoots_SkipsAStoreThatRecordedNoPath(t *testing.T) {
	db := oldShapeDBWithStores(t)
	insertBlockStore(t, db, "local-id", "local-mem", `{"type":"memory"}`)
	insertShare(t, db, "/scratch", "remote-id", "local-id")

	if err := checkShareJournalRoots(db, "/var/lib/dittofs/blocks"); err != nil {
		t.Fatalf("a store with no recorded path is not this guard's business, got %v", err)
	}
}

func TestJournalRootFromStoreConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		blob string
		want string
	}{
		{"empty blob", "", ""},
		{"unparseable blob", "not json", ""},
		{"no path key", `{"type":"memory"}`, ""},
		{"empty path", `{"path":""}`, ""},
		{"path", `{"path":"/srv/dittofs"}`, "/srv/dittofs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := journalRootFromStoreConfig(tc.blob); got != tc.want {
				t.Errorf("journalRootFromStoreConfig(%q) = %q, want %q", tc.blob, got, tc.want)
			}
		})
	}
}

func TestCheckJournalRoot_AcceptsMatchingPaths(t *testing.T) {
	err := checkJournalRoot("/srv/blocks", []shareJournalPath{
		{Share: "a", Path: "/srv/blocks"},
		{Share: "b", Path: "/srv/blocks/"}, // trailing separator is the same place
	})
	if err != nil {
		t.Fatalf("checkJournalRoot: got %v, want nil", err)
	}
}

func TestCheckJournalRoot_AcceptsNoRecordedPaths(t *testing.T) {
	if err := checkJournalRoot("/srv/blocks", nil); err != nil {
		t.Errorf("fresh install: got %v, want nil", err)
	}
	if err := checkJournalRoot("/srv/blocks", []shareJournalPath{{Share: "a"}}); err != nil {
		t.Errorf("share with no recorded path: got %v, want nil", err)
	}
}

// The caller decides refuse-vs-warn by matching the sentinel. If the returned
// error does not unwrap to it, a mismatched share is skipped with a warning and
// then mounted against the wrong directory, which reads back as zeros.
func TestCheckJournalRoot_ErrorUnwrapsToSentinel(t *testing.T) {
	err := checkJournalRoot("/srv/blocks", []shareJournalPath{{Share: "a", Path: "/mnt/other"}})
	if err == nil {
		t.Fatal("got nil, want a mismatch error")
	}
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("errors.Is(err, ErrJournalRootMismatch) = false; err = %v", err)
	}
}

// One shared location can be adopted by pointing the config at it.
func TestCheckJournalRoot_SingleDivergentPathSuggestsAdoptingIt(t *testing.T) {
	err := checkJournalRoot("/srv/blocks", []shareJournalPath{
		{Share: "a", Path: "/mnt/data"},
		{Share: "b", Path: "/mnt/data"},
	})
	if err == nil {
		t.Fatal("got nil, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "Set blockstore.journal.path to /mnt/data") {
		t.Errorf("error must tell the operator to adopt the shared path; got:\n%s", err)
	}
}

// Several locations cannot be expressed by one root, so the message must not
// suggest adopting one and stranding the rest.
func TestCheckJournalRoot_MultipleDivergentPathsDoNotSuggestOne(t *testing.T) {
	err := checkJournalRoot("/srv/blocks", []shareJournalPath{
		{Share: "b", Path: "/mnt/two"},
		{Share: "a", Path: "/mnt/one"},
	})
	if err == nil {
		t.Fatal("got nil, want a mismatch error")
	}
	msg := err.Error()
	if strings.Contains(msg, "Set blockstore.journal.path to") {
		t.Errorf("must not suggest adopting one of several paths; got:\n%s", msg)
	}
	for _, want := range []string{`share "a"`, "/mnt/one", `share "b"`, "/mnt/two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name %q; got:\n%s", want, msg)
		}
	}
	// Named in a stable order so the message does not churn between restarts.
	if strings.Index(msg, `share "a"`) > strings.Index(msg, `share "b"`) {
		t.Errorf("shares must be named in sorted order; got:\n%s", msg)
	}
}

func TestCheckJournalRoot_RefusesWhenNoRootConfigured(t *testing.T) {
	err := checkJournalRoot("", []shareJournalPath{{Share: "a", Path: "/mnt/data"}})
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("got %v, want a wrapped ErrJournalRootMismatch", err)
	}
}
