// Tests in this file verify CORRECTNESS by comparing output bytes against
// the input fixture. They do NOT verify which internal branch (mmap vs
// ReadFile) executed — that distinction is opaque from outside the
// function. The threshold-branch coverage (Test 5) is a behavioral
// assertion: a 1 KiB file produces correct bytes regardless of which
// branch ran. To verify mmap actually executes on linux/darwin (rather
// than silently falling through to ReadFile), use a trace-based or
// syscall-counter test in a separate harness — out of scope for Phase 12.

package engine

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeMmapFixture creates a temp file with the given content and returns
// its absolute path. Caller's t.TempDir() ensures cleanup. Named to avoid
// collision with perf_bench_test.go's writeFixture type.
func writeMmapFixture(t *testing.T, dir string, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", p, err)
	}
	return p
}

// randomBytes returns n cryptographically-random bytes.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// TestReadFromCAS_RoundTrip verifies a full-file read returns identical
// bytes (covers behaviors 1 + 4 — same code path on all platforms; the
// linux/darwin branch uses mmap, windows uses ReadFile).
func TestReadFromCAS_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Use a size above the 64 KiB mmap threshold so the unix branch
	// exercises the mmap path (windows always uses ReadFile).
	const sz = 256 * 1024
	want := randomBytes(t, sz)
	path := writeMmapFixture(t, dir, "block.dat", want)

	got := make([]byte, sz)
	n, err := readFromCAS(path, 0, got)
	if err != nil {
		t.Fatalf("readFromCAS: %v", err)
	}
	if n != sz {
		t.Fatalf("n=%d want %d", n, sz)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes mismatch: got len %d, want len %d", len(got), len(want))
	}
}

// TestReadFromCAS_PartialOffset verifies offset-based read returns the
// expected suffix of the fixture.
func TestReadFromCAS_PartialOffset(t *testing.T) {
	dir := t.TempDir()
	const sz = 256 * 1024
	want := randomBytes(t, sz)
	path := writeMmapFixture(t, dir, "block.dat", want)

	const off = 1024
	got := make([]byte, sz-off)
	n, err := readFromCAS(path, off, got)
	if err != nil {
		t.Fatalf("readFromCAS: %v", err)
	}
	if n != sz-off {
		t.Fatalf("n=%d want %d", n, sz-off)
	}
	if !bytes.Equal(got, want[off:]) {
		t.Fatalf("bytes mismatch at offset %d", off)
	}
}

// TestReadFromCAS_DestSmallerThanFile verifies copy is bounded by
// len(dest) (not by file size). 1 MiB fixture, 4 KiB dest → returns 4096.
func TestReadFromCAS_DestSmallerThanFile(t *testing.T) {
	dir := t.TempDir()
	const sz = 1 << 20 // 1 MiB
	want := randomBytes(t, sz)
	path := writeMmapFixture(t, dir, "block.dat", want)

	got := make([]byte, 4096)
	n, err := readFromCAS(path, 0, got)
	if err != nil {
		t.Fatalf("readFromCAS: %v", err)
	}
	if n != 4096 {
		t.Fatalf("n=%d want 4096", n)
	}
	if !bytes.Equal(got, want[:4096]) {
		t.Fatalf("bytes mismatch on bounded read")
	}
}

// TestReadFromCAS_BelowMmapThreshold_UsesReadFile verifies behavioral
// correctness when the fixture is below the mmap threshold (64 KiB on
// linux/darwin). Result must be byte-identical regardless of internal
// branch (mmap vs ReadFile).
func TestReadFromCAS_BelowMmapThreshold_UsesReadFile(t *testing.T) {
	dir := t.TempDir()
	const sz = 1024 // 1 KiB — below 64 KiB threshold
	want := randomBytes(t, sz)
	path := writeMmapFixture(t, dir, "block.dat", want)

	got := make([]byte, sz)
	n, err := readFromCAS(path, 0, got)
	if err != nil {
		t.Fatalf("readFromCAS: %v", err)
	}
	if n != sz {
		t.Fatalf("n=%d want %d", n, sz)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes mismatch on below-threshold read")
	}

	// Partial-offset on below-threshold file.
	const off = 256
	got2 := make([]byte, sz-off)
	n2, err := readFromCAS(path, off, got2)
	if err != nil {
		t.Fatalf("readFromCAS: %v", err)
	}
	if n2 != sz-off {
		t.Fatalf("n=%d want %d", n2, sz-off)
	}
	if !bytes.Equal(got2, want[off:]) {
		t.Fatalf("bytes mismatch on below-threshold partial read")
	}
}

// TestReadFromCAS_OffsetAtEOF returns 0 (no error) when offset >= file size.
func TestReadFromCAS_OffsetAtEOF(t *testing.T) {
	dir := t.TempDir()
	const sz = 256 * 1024
	path := writeMmapFixture(t, dir, "block.dat", randomBytes(t, sz))

	got := make([]byte, 4096)
	n, err := readFromCAS(path, sz, got)
	if err != nil {
		t.Fatalf("readFromCAS: %v", err)
	}
	if n != 0 {
		t.Fatalf("n=%d want 0 at EOF offset", n)
	}
}

// TestReadFromCAS_EmptyDest returns 0 immediately, regardless of file.
func TestReadFromCAS_EmptyDest(t *testing.T) {
	dir := t.TempDir()
	path := writeMmapFixture(t, dir, "block.dat", randomBytes(t, 1024))

	n, err := readFromCAS(path, 0, nil)
	if err != nil {
		t.Fatalf("readFromCAS empty dest: %v", err)
	}
	if n != 0 {
		t.Fatalf("n=%d want 0 with nil dest", n)
	}

	n, err = readFromCAS(path, 0, []byte{})
	if err != nil {
		t.Fatalf("readFromCAS zero-len dest: %v", err)
	}
	if n != 0 {
		t.Fatalf("n=%d want 0 with zero-len dest", n)
	}
}

// TestReadFromCAS_MissingFile returns an error.
func TestReadFromCAS_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got := make([]byte, 4096)
	_, err := readFromCAS(filepath.Join(dir, "does-not-exist"), 0, got)
	if err == nil {
		t.Fatalf("expected error reading missing file")
	}
}

// TestReadFromCAS_Windows_FallbackPath is a placeholder to document
// that the windows build-tagged sibling implements the same contract
// using os.ReadFile. Skipped on non-windows runners.
func TestReadFromCAS_Windows_FallbackPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only fallback path; the function under test is build-tagged sibling")
	}
	// On windows GOOS the round-trip test above already exercises the
	// ReadFile branch — no separate behavior to assert here.
}
