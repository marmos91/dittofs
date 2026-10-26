package prof

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWatchFailsOnReusedDescriptor checks that a descriptor counts as leaked
// when the test closed one it inherited and opened another under the same
// number, which a comparison of numbers alone would miss. It runs on Linux
// only, the one platform where the check reads the file a descriptor names.
func TestWatchFailsOnReusedDescriptor(t *testing.T) {
	dir := t.TempDir()

	a, err := os.Create(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}

	var b *os.File
	t.Cleanup(func() {
		if b != nil {
			b.Close()
		}
	})

	f := newFakeTB(t)
	Watch(f, Options{})

	fd := a.Fd()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	pathB := filepath.Join(dir, "b")
	b, err = os.Create(pathB)
	if err != nil {
		t.Fatal(err)
	}

	if b.Fd() != fd {
		t.Fatalf("b opened as fd %d, want a's fd %d: the plant does not reuse the number", b.Fd(), fd)
	}

	f.end()

	if got := f.errorsContaining(pathB); len(got) != 1 || len(f.errors) != 1 {
		t.Errorf("errors = %q, want one naming %s", f.errors, pathB)
	}

	assertProfiles(t, f.artifacts)
}
