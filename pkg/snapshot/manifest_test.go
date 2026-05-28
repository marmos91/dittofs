package snapshot_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// mustHash returns a deterministic ContentHash seeded by byteSeed so
// every test gets unique, sortable hashes without RNG flakiness.
func mustHash(byteSeed byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	for i := range h {
		h[i] = byteSeed + byte(i)
	}
	return h
}

func TestSkeleton_SentinelExists(t *testing.T) {
	if snapshot.ErrInvalidManifestLine == nil {
		t.Fatal("snapshot.ErrInvalidManifestLine must be a non-nil sentinel")
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 1000} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			hs := blockstore.NewHashSet(n)
			originals := make([]blockstore.ContentHash, 0, n)
			for i := 0; i < n; i++ {
				h := mustHash(byte(i))
				// disambiguate beyond 256 seeds
				h[0] = byte(i)
				h[1] = byte(i >> 8)
				hs.Add(h)
				originals = append(originals, h)
			}

			var buf bytes.Buffer
			if err := snapshot.WriteManifest(&buf, hs); err != nil {
				t.Fatalf("WriteManifest: %v", err)
			}

			got, err := snapshot.ReadManifest(&buf)
			if err != nil {
				t.Fatalf("ReadManifest: %v", err)
			}
			if got.Len() != hs.Len() {
				t.Fatalf("Len mismatch: got %d want %d", got.Len(), hs.Len())
			}
			for _, h := range originals {
				if !got.Contains(h) {
					t.Fatalf("missing hash %s after round-trip", h.String())
				}
			}
		})
	}
}

func TestWriteRead_SortedAscending(t *testing.T) {
	hs := blockstore.NewHashSet(5)
	// Insert in non-ascending seed order to prove the writer re-sorts via
	// HashSet.Sorted (the map iteration order is unspecified anyway).
	for _, seed := range []byte{0x40, 0x10, 0x80, 0x20, 0x60} {
		hs.Add(mustHash(seed))
	}

	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, hs); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	body := buf.String()
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("manifest must end in newline; got %q", body)
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1] >= lines[i] {
			t.Fatalf("lines not strictly ascending at %d: %q >= %q", i, lines[i-1], lines[i])
		}
	}
}

func TestRead_RejectsMalformed(t *testing.T) {
	good := mustHash(0xAA).String() // 64 hex chars
	cases := map[string]string{
		"short_63":   good[:63] + "\n",
		"long_65":    good + "a\n",
		"non_hex":    strings.Repeat("Z", 64) + "\n",
		"with_space": good[:62] + " a" + "\n",
	}

	for name, payload := range cases {
		name := name
		payload := payload
		t.Run(name, func(t *testing.T) {
			_, err := snapshot.ReadManifest(strings.NewReader(payload))
			if err == nil {
				t.Fatalf("expected error for %s, got nil", name)
			}
			if !errors.Is(err, snapshot.ErrInvalidManifestLine) {
				t.Fatalf("error %v does not match ErrInvalidManifestLine", err)
			}
			if !strings.Contains(err.Error(), "line 1") {
				t.Fatalf("error %q missing line number", err.Error())
			}
		})
	}

	// Malformed line on line 3 must surface line=3 in the message.
	// Use distinct good lines so HashSet collapse cannot confuse line numbering.
	good2 := mustHash(0xBB).String()
	body := good + "\n" + good2 + "\n" + "deadbeef\n"
	_, err := snapshot.ReadManifest(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for malformed line 3")
	}
	if !errors.Is(err, snapshot.ErrInvalidManifestLine) {
		t.Fatalf("expected ErrInvalidManifestLine, got %v", err)
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("expected line 3 in error, got %q", err.Error())
	}
}

func TestRead_EmptyInput_ReturnsEmptySet(t *testing.T) {
	got, err := snapshot.ReadManifest(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
	if got == nil {
		t.Fatal("ReadManifest returned nil HashSet")
	}
	if got.Len() != 0 {
		t.Fatalf("expected empty HashSet, got Len=%d", got.Len())
	}
}

func TestRead_ToleratesCRLF(t *testing.T) {
	h := mustHash(0x33)
	body := h.String() + "\r\n"
	got, err := snapshot.ReadManifest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("CRLF input rejected: %v", err)
	}
	if got.Len() != 1 || !got.Contains(h) {
		t.Fatalf("CRLF round-trip lost the hash")
	}
}

// TestRead_LargeBuffer_Handles100k is the load-bearing regression guard
// for the explicit sc.Buffer sizing in ReadManifest. At 100k hashes the
// payload is ~6.4 MiB; the default scanner would still cope here (each
// line is 64 chars), but exercising the round-trip at realistic
// snapshot scale pins behaviour against future input-shape regressions.
func TestRead_LargeBuffer_Handles100k(t *testing.T) {
	const N = 100_000
	hs := blockstore.NewHashSet(N)
	for i := 0; i < N; i++ {
		var h blockstore.ContentHash
		// Distribute uniquely across 100k by mixing in 3 byte positions.
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		h[2] = byte(i >> 16)
		hs.Add(h)
	}

	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, hs); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := snapshot.ReadManifest(&buf)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Len() != N {
		t.Fatalf("Len mismatch: got %d want %d", got.Len(), N)
	}
	for _, idx := range []int{0, N / 2, N - 1} {
		var h blockstore.ContentHash
		h[0] = byte(idx)
		h[1] = byte(idx >> 8)
		h[2] = byte(idx >> 16)
		if !got.Contains(h) {
			t.Fatalf("missing sampled hash at idx=%d", idx)
		}
	}
}

func TestWriteAtomic_CompleteFileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.hashes")

	hs := blockstore.NewHashSet(3)
	for _, s := range []byte{0x01, 0x02, 0x03} {
		hs.Add(mustHash(s))
	}

	if err := snapshot.WriteManifestAtomic(path, hs); err != nil {
		t.Fatalf("WriteManifestAtomic: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("canonical path missing: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected .tmp to be absent post-success, got err=%v", err)
	}

	// Confirm contents round-trip.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	got, err := snapshot.ReadManifest(f)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Len() != 3 {
		t.Fatalf("round-trip len mismatch: got %d want 3", got.Len())
	}
}

func TestWriteAtomic_NoPartialOnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics required")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o500 dir permissions")
	}

	dir := t.TempDir()
	roDir := filepath.Join(dir, "readonly")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatalf("mkdir readonly: %v", err)
	}
	// Re-grant write at the end so t.TempDir cleanup succeeds.
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	path := filepath.Join(roDir, "snap.hashes")
	hs := blockstore.NewHashSet(1)
	hs.Add(mustHash(0x77))

	err := snapshot.WriteManifestAtomic(path, hs)
	if err == nil {
		t.Fatal("expected WriteManifestAtomic to fail on read-only dir")
	}

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("canonical path must not exist on failure, stat err=%v", statErr)
	}
}

func TestWriteAtomic_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.hashes")

	first := blockstore.NewHashSet(1)
	first.Add(mustHash(0x10))
	if err := snapshot.WriteManifestAtomic(path, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	second := blockstore.NewHashSet(2)
	second.Add(mustHash(0x20))
	second.Add(mustHash(0x30))
	if err := snapshot.WriteManifestAtomic(path, second); err != nil {
		t.Fatalf("second write: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	got, err := snapshot.ReadManifest(f)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("expected second manifest (len 2), got len=%d", got.Len())
	}
	if got.Contains(mustHash(0x10)) {
		t.Fatal("first manifest contents leaked into second")
	}
	if !got.Contains(mustHash(0x20)) || !got.Contains(mustHash(0x30)) {
		t.Fatal("second manifest hashes missing")
	}
}

// TestWriteRead_DuplicateLinesCollapse pins the HashSet de-dup behavior
// for hand-edited or maliciously crafted manifests: duplicate lines do
// NOT raise an error, they collapse into a single set entry.
func TestWriteRead_DuplicateLinesCollapse(t *testing.T) {
	h := mustHash(0x55)
	body := h.String() + "\n" + h.String() + "\n" + h.String() + "\n"
	got, err := snapshot.ReadManifest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("dup lines rejected: %v", err)
	}
	if got.Len() != 1 || !got.Contains(h) {
		t.Fatalf("dup lines did not collapse: len=%d contains=%v", got.Len(), got.Contains(h))
	}
}
