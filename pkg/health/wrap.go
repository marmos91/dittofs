package health

import (
	"context"
	"time"
)

// ReportFromError synthesises a [Report] from an error-returning probe
// result. This is the bridge between the legacy `func(ctx) error` health
// check style used inside dittofs and the new [Report]-returning
// [Checker] interface that downstream consumers (the API layer, the
// CLI, the Pro UI) want to see.
//
// A nil error becomes [StatusHealthy] with an empty message. A non-nil
// error becomes [StatusUnhealthy] with the error string as the message.
// The CheckedAt and LatencyMs fields are populated from the supplied
// arguments — the caller is responsible for measuring them around the
// probe call.
//
// For more nuanced status derivation (e.g. mapping a specific sentinel
// error to [StatusDegraded] rather than [StatusUnhealthy]), build the
// [Report] manually instead of using this helper.
func ReportFromError(err error, latency time.Duration) Report {
	rep := Report{
		CheckedAt: time.Now().UTC(),
		LatencyMs: latency.Milliseconds(),
	}
	if err != nil {
		rep.Status = StatusUnhealthy
		rep.Message = err.Error()
		return rep
	}
	rep.Status = StatusHealthy
	return rep
}

// CheckerFromErrorFunc adapts a legacy `func(ctx) error` probe into a
// [Checker]. The returned Checker measures wall-clock latency around
// the probe and uses [ReportFromError] to build the report.
//
// Useful for wrapping store and adapter implementations that already
// expose an error-returning healthcheck method without rewriting the
// implementation. Implementations that want richer status information
// (e.g. distinguishing degraded from unhealthy) should implement the
// [Checker] interface directly instead.
func CheckerFromErrorFunc(probe func(ctx context.Context) error) Checker {
	return CheckerFunc(func(ctx context.Context) Report {
		start := time.Now()
		err := probe(ctx)
		return ReportFromError(err, time.Since(start))
	})
}
