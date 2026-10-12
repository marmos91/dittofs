package kvstore_test

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

func names(t *testing.T, seq func(func(kvstore.Listed, error) bool)) ([]string, []uint64) {
	t.Helper()
	var ns []string
	var cs []uint64
	for l, err := range seq {
		must(t, err)
		ns = append(ns, string(l.Entry.Name))
		cs = append(cs, l.Cookie)
	}
	return ns, cs
}

func TestEntries(t *testing.T) {
	f := newFS(t, entity.FoldUpcase)
	ns := f.s.Namespace()
	const n = 600 // more than two pages
	made := map[string]entity.FileID{}
	for i := range n {
		name := fmt.Sprintf("File-%03d", i)
		_, file := f.create(f.admin, f.root, name, regular(0o644))
		made[name] = file.ID
	}
	got, cookies := names(t, ns.Entries(ctx, f.admin, f.root, 0))
	if len(got) != n {
		t.Fatalf("%d entries, want %d", len(got), n)
	}
	seen := map[string]bool{}
	for i, name := range got {
		if seen[name] || made[name] == (entity.FileID{}) {
			t.Fatalf("%q repeated or unknown", name)
		}
		seen[name] = true
		c := cookies[i]
		if c < 3 || c>>63 != 0 || (i > 0 && c < cookies[i-1]) {
			t.Fatalf("cookie %#x at %d", c, i)
		}
	}
	// Names come back as given, not folded.
	if !seen["File-000"] {
		t.Error("name not returned as given")
	}
	// Resuming after any cookie returns exactly the entries after it.
	rest, _ := names(t, ns.Entries(ctx, f.admin, f.root, cookies[299]))
	if len(rest) != n-300 || rest[0] != got[300] {
		t.Fatalf("resume: %d entries starting %q", len(rest), rest[0])
	}
	// EntriesPlus carries each child's record.
	for l, err := range ns.EntriesPlus(ctx, f.admin, f.root, cookies[n-3]) {
		must(t, err)
		if l.File.ID != l.Entry.Child || l.File.ID != made[string(l.Entry.Name)] {
			t.Errorf("%s: file %v", l.Entry.Name, l.File.ID)
		}
	}
	// Listing needs the read right on the directory.
	priv, _ := f.create(f.admin, f.root, "private", directory(0o700))
	stranger := f.member("stranger", entity.Principal{})
	for _, err := range ns.Entries(ctx, stranger, priv, 0) {
		if !isDenied(err) {
			t.Errorf("listing a 0700 directory: %v", err)
		}
	}
}
