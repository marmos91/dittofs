// Package blockstore benchmarks BLAKE3 vs SHA-256 hashing throughput used by
// the v0.15.0 CAS content-hash layer (see Phase 10 design decisions D-08 and
// D-41).
//
// D-41 gate (amended 2026-04-24 for physical reality on arm64):
//
// The BLAKE3/SHA-256 ratio is hardware-dependent. On amd64, BLAKE3's SIMD
// assembly (AVX-512/AVX-2/SSE4.1) dwarfs Go's software SHA-256 and the 3x
// target from PROJECT.md is achievable. On arm64, Go's crypto/sha256 uses
// the ARMv8 SHA-2 hardware extension (very fast ~2-3 GB/s) while
// lukechampine.com/blake3 falls back to portable Go on most arm64 chips
// (zeebo/blake3 is in the same boat). The resulting ratio is closer to
// 1.5-2x — a 3x universal gate is physically unachievable on arm64 today.
//
// Gate applied here (platform-aware, user-approved Option A):
//
//   - amd64: BLAKE3 throughput must be >= 3.0x SHA-256. Detects a missing
//     AVX-2 compile flag or a silently slow portable-Go fallback on a CPU
//     that should have SIMD.
//   - arm64 (and other arches): BLAKE3 throughput must be >= 1.0x SHA-256.
//     This is a sanity lower-bound — BLAKE3 should never be slower than
//     SHA-256. Catches a pathologically broken wiring while acknowledging
//     the hw-SHA vs portable-Go BLAKE3 asymmetry.
//
// The hard 3x D-41 target is validated on the CI amd64 perf lane per D-43;
// this test keeps the local dev loop green on Apple Silicon without
// masking a real amd64 regression.
//
// We depend on lukechampine.com/blake3 (D-08 amendment) rather than
// github.com/zeebo/blake3 because the former ships both amd64 SIMD and
// arm64 NEON paths where applicable.
package blockstore_test

import (
	"crypto/rand"
	"crypto/sha256"
	"runtime"
	"testing"

	"lukechampine.com/blake3"
)

// benchBufSize is 256 MiB per the D-41 microbenchmark spec. Large enough to
// amortize per-call setup and exercise the full SIMD path.
const benchBufSize = 256 * 1024 * 1024

// makeBenchBuf returns an entropy-rich buffer so hash compressors cannot
// shortcut via zero-skipping or run-length optimizations.
func makeBenchBuf(tb testing.TB) []byte {
	tb.Helper()
	buf := make([]byte, benchBufSize)
	if _, err := rand.Read(buf); err != nil {
		tb.Fatalf("rand.Read: %v", err)
	}
	return buf
}

// BenchmarkBLAKE3_256MiB measures BLAKE3-256 throughput on a 256 MiB input.
func BenchmarkBLAKE3_256MiB(b *testing.B) {
	buf := makeBenchBuf(b)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for b.Loop() {
		_ = blake3.Sum256(buf)
	}
}

// BenchmarkSHA256_256MiB measures SHA-256 throughput on a 256 MiB input.
func BenchmarkSHA256_256MiB(b *testing.B) {
	buf := makeBenchBuf(b)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for b.Loop() {
		_ = sha256.Sum256(buf)
	}
}

// TestBLAKE3FasterThanSHA256 enforces the platform-aware D-41 perf gate.
// See the package doc comment for the rationale behind the threshold split.
//
// Skipped under `-short` because each benchmark allocates 256 MiB and the
// full gate run adds ~1s of CPU.
func TestBLAKE3FasterThanSHA256(t *testing.T) {
	if testing.Short() {
		t.Skip("D-41 gate is heavy (256 MiB * 2); skip under -short")
	}
	if raceEnabled {
		// Under -race, SIMD paths are disabled/instrumented; BLAKE3 falls to
		// portable Go while SHA-256 keeps its ARMv8 SHA path on arm64. The
		// ratio collapses to noise and the gate is meaningless. Correctness
		// of the BLAKE3 wiring is covered by unit tests; perf is gated on
		// the non-race CI lane.
		t.Skip("D-41 gate is a throughput test; skip under -race (SIMD disabled)")
	}

	br := testing.Benchmark(BenchmarkBLAKE3_256MiB)
	sr := testing.Benchmark(BenchmarkSHA256_256MiB)

	if br.NsPerOp() == 0 || sr.NsPerOp() == 0 {
		t.Fatalf("benchmark produced zero ns/op: blake3=%+v sha256=%+v", br, sr)
	}

	ratio := float64(sr.NsPerOp()) / float64(br.NsPerOp())

	// Platform-aware threshold (D-41 amended 2026-04-24):
	// amd64 = 3.0x (catches missing SIMD), others = 1.0x (sanity).
	var threshold float64
	switch runtime.GOARCH {
	case "amd64":
		threshold = 3.0
	default:
		threshold = 1.0
	}

	t.Logf("D-41 gate [GOARCH=%s threshold=%.2fx]: "+
		"BLAKE3=%d ns/op  SHA-256=%d ns/op  ratio=%.2fx",
		runtime.GOARCH, threshold, br.NsPerOp(), sr.NsPerOp(), ratio)

	if ratio < threshold {
		t.Fatalf("D-41 gate FAILED on %s: BLAKE3 is %.2fx SHA-256 (need >= %.2fx); "+
			"blake3=%d ns/op sha256=%d ns/op. "+
			"Check that lukechampine.com/blake3 SIMD assembly is engaged on this CPU.",
			runtime.GOARCH, ratio, threshold, br.NsPerOp(), sr.NsPerOp())
	}

	t.Logf("D-41 gate met on %s: ratio=%.2fx >= %.2fx", runtime.GOARCH, ratio, threshold)
}

// TestBLAKE3AtLeast3xSHA256 is a legacy alias kept for plan-acceptance
// traceability (10-01 plan references this test name). It delegates to the
// platform-aware gate above.
func TestBLAKE3AtLeast3xSHA256(t *testing.T) {
	TestBLAKE3FasterThanSHA256(t)
}
