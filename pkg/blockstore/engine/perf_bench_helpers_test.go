package engine

import (
	"testing"

	"github.com/marmos91/dittofs/internal/logger"
)

// silenceLoggerForBench drops log level to ERROR for the duration of a
// benchmark / gate test so per-iteration INFO lines don't pollute the
// bench output (which makes the ns/op line unparseable for downstream
// tooling). Restored on cleanup.
func silenceLoggerForBench(tb testing.TB) {
	tb.Helper()
	logger.SetLevel("ERROR")
	tb.Cleanup(func() { logger.SetLevel("INFO") })
}

// reportOpsPerSec emits an "ops/s" custom metric so `go test -bench`
// output carries the IOPS figure directly (alongside ns/op / B/op /
// allocs/op). We also re-derive it from b.Elapsed so the metric is
// exact even when the loop body has variable cost.
func reportOpsPerSec(b *testing.B, ops int) {
	b.Helper()
	elapsed := b.Elapsed().Seconds()
	if elapsed <= 0 {
		return
	}
	b.ReportMetric(float64(ops)/elapsed, "ops/s")
}
