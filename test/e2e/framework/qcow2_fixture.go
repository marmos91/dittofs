//go:build e2e

// Phase 13 DEDUP-03 (VER-03 gate) qcow2 fixture: pinned-URL download +
// SHA256 verify + deterministic clone synthesis. See
// test/e2e/fixtures/qcow2/README.md for the fixture rationale, and
// .planning/phases/13-merkle-root-file-level-dedup-a4/13-CONTEXT.md
// decision D-15 for the >=40% storage-reduction gate.
package framework

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	// qcow2BaseURL is the pinned upstream URL for the Alpine cloud
	// qcow2 used by TestDEDUP03_VMFleet40Pct. ~50 MiB; downloads once
	// per CI host into qcow2BaseCachePath and is re-verified against
	// qcow2BaseSHA256 on every test run.
	qcow2BaseURL = "https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/nocloud_alpine-3.20.3-x86_64-bios-cloudinit-r0.qcow2"

	// qcow2BaseSHA256 is the pinned content digest. Sentinel
	// "<freeze-at-first-run>" defers the pin to the executor: on the
	// first nightly run, DownloadQcow2Base logs the observed digest and
	// the executor freezes it here in a follow-up commit. Once frozen,
	// any upstream rotation (T-13-20) fails the test loudly.
	qcow2BaseSHA256 = "<freeze-at-first-run>"

	// qcow2BaseCachePath is relative to the test working directory
	// (test/e2e when run via run-e2e.sh). Gitignored — see
	// fixtures/qcow2/.gitignore.
	qcow2BaseCachePath = "fixtures/qcow2/base.qcow2"

	// qcow2DownloadTimeout caps the first-time download. Alpine's CDN
	// is fast in normal conditions; bound the window to surface CI
	// network issues rather than letting them stall the suite.
	qcow2DownloadTimeout = 5 * time.Minute
)

// qcow2SHAPlaceholder is the value qcow2BaseSHA256 holds before the
// first run freezes a real digest. Kept as a constant so the freeze-on-
// first-run logic is grep-greppable.
const qcow2SHAPlaceholder = "<freeze-at-first-run>"

// DownloadQcow2Base returns the path to the cached qcow2 base image,
// downloading it on first call and verifying SHA256. Subsequent calls
// short-circuit on a SHA-match against the cached file.
//
// Behavior on SHA mismatch:
//   - Cached file with mismatching SHA256 -> re-download (treat as stale).
//   - Newly-downloaded file with mismatching SHA256 -> t.Fatalf
//     (T-13-20 mitigation: pinned upstream rotated unexpectedly).
//
// Freeze-on-first-run:
//   - When qcow2BaseSHA256 == qcow2SHAPlaceholder, the SHA check is
//     non-fatal; the observed digest is logged so the executor can
//     freeze it into the constant. The plan SUMMARY records the
//     frozen digest.
func DownloadQcow2Base(t *testing.T) string {
	t.Helper()
	dir := filepath.Dir(qcow2BaseCachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("DownloadQcow2Base: mkdir %q: %v", dir, err)
	}

	if cached, err := sha256OfFile(qcow2BaseCachePath); err == nil {
		if qcow2BaseSHA256 != qcow2SHAPlaceholder && cached == qcow2BaseSHA256 {
			return qcow2BaseCachePath
		}
		if qcow2BaseSHA256 == qcow2SHAPlaceholder {
			t.Logf("DownloadQcow2Base: cached qcow2 present (sha256=%s); pin this in qcow2BaseSHA256",
				cached)
			return qcow2BaseCachePath
		}
		// Stale cache: SHA mismatch and we're past the freeze window.
		t.Logf("DownloadQcow2Base: cached SHA mismatch (got %s want %s); re-downloading",
			cached, qcow2BaseSHA256)
	}

	client := &http.Client{Timeout: qcow2DownloadTimeout}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, qcow2BaseURL, nil)
	if err != nil {
		t.Fatalf("DownloadQcow2Base: build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DownloadQcow2Base: http.Get %q: %v", qcow2BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DownloadQcow2Base: %q status %d", qcow2BaseURL, resp.StatusCode)
	}

	tmpPath := qcow2BaseCachePath + ".tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		t.Fatalf("DownloadQcow2Base: create %q: %v", tmpPath, err)
	}
	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)
	n, err := io.Copy(mw, resp.Body)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("DownloadQcow2Base: io.Copy: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("DownloadQcow2Base: sync: %v", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("DownloadQcow2Base: close: %v", err)
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	switch qcow2BaseSHA256 {
	case qcow2SHAPlaceholder:
		t.Logf("DownloadQcow2Base: PIN this SHA256 in test/e2e/framework/qcow2_fixture.go: "+
			"qcow2BaseSHA256 = %q (downloaded %d bytes from %s)",
			actual, n, qcow2BaseURL)
	default:
		if actual != qcow2BaseSHA256 {
			_ = os.Remove(tmpPath)
			t.Fatalf("DownloadQcow2Base: SHA256 mismatch on download from %q: "+
				"got %s want %s (T-13-20: upstream may have rotated)",
				qcow2BaseURL, actual, qcow2BaseSHA256)
		}
	}

	if err := os.Rename(tmpPath, qcow2BaseCachePath); err != nil {
		t.Fatalf("DownloadQcow2Base: rename %q -> %q: %v", tmpPath, qcow2BaseCachePath, err)
	}
	return qcow2BaseCachePath
}

// SynthesizeClones generates `n` deterministic clones of the base image
// in `outDir`. Each clone = base bytes + ~7.5% per-clone random patches
// at seeded offsets, applied as 4-32 KiB localized writes (FastCDC
// re-stabilizes after max-chunk = 16 MiB, so localized patches are kind
// to dedup).
//
// Seed for clone i is `int64(i) ^ 0xDEDD0F` so clones are reproducible
// across runs but distinct from one another. Returns the absolute (or
// outDir-relative) clone paths.
func SynthesizeClones(t *testing.T, basePath, outDir string, n int) []string {
	t.Helper()
	base, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("SynthesizeClones: read base %q: %v", basePath, err)
	}
	if len(base) == 0 {
		t.Fatalf("SynthesizeClones: base file %q is empty", basePath)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("SynthesizeClones: mkdir %q: %v", outDir, err)
	}

	const targetDivergencePct = 0.075 // ~7.5% of base length per clone
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		clone := make([]byte, len(base))
		copy(clone, base)
		seed := int64(i) ^ 0xDEDD0F
		r := rand.New(rand.NewSource(seed))

		targetDivergence := int(targetDivergencePct * float64(len(clone)))
		written := 0
		for written < targetDivergence {
			patchLen := 4*1024 + r.Intn(28*1024) // 4 KiB .. 32 KiB
			if patchLen > targetDivergence-written {
				patchLen = targetDivergence - written
			}
			if patchLen <= 0 || patchLen >= len(clone) {
				break
			}
			offset := r.Intn(len(clone) - patchLen)
			patch := make([]byte, patchLen)
			if _, err := r.Read(patch); err != nil {
				t.Fatalf("SynthesizeClones: rand.Read: %v", err)
			}
			copy(clone[offset:], patch)
			written += patchLen
		}

		// Two-digit zero-padded so directory listings sort sensibly
		// when debugging fixture output.
		path := filepath.Join(outDir, fmt.Sprintf("clone-%02d.qcow2", i))
		if err := os.WriteFile(path, clone, 0o644); err != nil {
			t.Fatalf("SynthesizeClones: write %q: %v", path, err)
		}
		paths[i] = path
	}
	return paths
}

// sha256OfFile returns the hex SHA256 of the file at path. Returns an
// error if the file cannot be read.
func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
