package known

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Prune removes the entries keep refuses and leaves the rest of the file as
// it was. A removed entry's comment heads the entries after it, so the last
// such comment moves to the next entry kept, and goes with the last entry it
// headed. A file with nothing to remove isn't written.
func TestPrune(t *testing.T) {
	const file = "# The old failures.\n" +
		"- {test: a, status: fail, fix: wontfix, category: proto, reason: gone}\n" +
		"# The failures.\n" +
		"- {test: c, status: fail, fix: wontfix, category: proto, reason: gone}\n" +
		"- test: b\n  status: fail\n  fix: planned\n  category: bug\n  reason: 'still: failing'\n  issue: '#1'\n  where: {nfs: [\"3\"]}\n" +
		"# The skips.\n" +
		"- {test: d, status: skip, fix: wontfix, category: env, reason: kept}\n" +
		"# The last skip.\n" +
		"- {test: e, status: skip, fix: wontfix, category: env, reason: gone}\n"
	path := write(t, filepath.Join(t.TempDir(), "known.yaml"), file)

	removed, err := Prune(path, func(e Entry) bool { return e.Reason != "gone" })
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range removed {
		names = append(names, e.Test)
	}
	if !slices.Equal(names, []string{"a", "c", "e"}) {
		t.Errorf("removed %q, want a, c and e", names)
	}
	want := "# The failures.\n" +
		"- test: b\n  status: fail\n  fix: planned\n  category: bug\n  reason: 'still: failing'\n  issue: '#1'\n  where: {nfs: [\"3\"]}\n" +
		"# The skips.\n" +
		"- {test: d, status: skip, fix: wontfix, category: env, reason: kept}\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("file\n%s\nwant\n%s", got, want)
	}

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if removed, err := Prune(path, func(Entry) bool { return true }); err != nil || removed != nil {
		t.Errorf("nothing to remove: %v, %v", removed, err)
	}
}
