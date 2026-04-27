//go:build !race

package engine

// raceEnabled reports whether the test binary was built with -race. Used by
// TestPerfGate_VerifierWithinBudget to skip the benchmark-driven perf gate
// when race instrumentation is active — race-mode ns/op is dominated by
// shadow-memory bookkeeping and bears no relation to production throughput,
// so the gate's measurement is meaningless and its 9+ minute runtime
// reliably trips the default 10-minute test timeout in CI.
const raceEnabled = false
