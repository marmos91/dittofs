package prof

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchFailsOnADescriptorNumberReusedForAnotherFile(t *testing.T) {
	shortSettle(t)
	dir := t.TempDir()
	inherited, err := os.Create(filepath.Join(dir, "inherited"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inherited.Close() })
	rec := newRecorder(t)

	Watch(rec, Options{})
	fd := inherited.Fd()
	if err := inherited.Close(); err != nil {
		t.Fatal(err)
	}
	reused, err := os.Create(filepath.Join(dir, "reused"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reused.Close() })
	if reused.Fd() != fd {
		t.Fatalf("the new file opened as descriptor %d, not the closed one's %d", reused.Fd(), fd)
	}
	rec.finish()

	if !strings.Contains(rec.failure(), "reused") {
		t.Errorf("a leaked file under a number the test inherited did not fail the watch:\n%s", rec.failure())
	}
}

func TestWatchPassesAFileRemovedWhileADescriptorOpenedBeforeItHoldsIt(t *testing.T) {
	shortSettle(t)
	path := filepath.Join(t.TempDir(), "removed")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	rec := newRecorder(t)

	Watch(rec, Options{})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rec.finish()

	if rec.Failed() {
		t.Errorf("removing a file a descriptor from before the watch holds failed the watch:\n%s", rec.failure())
	}
}
