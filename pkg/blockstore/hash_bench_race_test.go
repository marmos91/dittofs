//go:build race

package blockstore_test

// raceEnabled reports whether the binary was built with -race.
// Under -race, SIMD assembly paths (lukechampine/blake3 AVX/NEON and
// Go's crypto/sha256 hw-accel) are disabled or heavily instrumented,
// which collapses throughput ratios to noise. The D-41 perf gate is
// inherently meaningless under -race and self-skips via this flag.
const raceEnabled = true
