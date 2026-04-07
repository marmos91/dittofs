package health

import (
	"context"
	"time"
)

// NewHealthyReport returns a [StatusHealthy] [Report] stamped with the
// current UTC time and the supplied probe latency. Use this from store
// implementations whose Healthcheck method has measured wall-clock time
// around its probe and just needs to package the result.
func NewHealthyReport(latency time.Duration) Report {
	return Report{
		Status:    StatusHealthy,
		CheckedAt: time.Now().UTC(),
		LatencyMs: latency.Milliseconds(),
	}
}

// NewUnhealthyReport returns a [StatusUnhealthy] [Report] with the
// supplied operator-facing message, stamped with the current UTC time
// and the supplied probe latency. The companion of [NewHealthyReport]
// for the failure path.
func NewUnhealthyReport(msg string, latency time.Duration) Report {
	return Report{
		Status:    StatusUnhealthy,
		Message:   msg,
		CheckedAt: time.Now().UTC(),
		LatencyMs: latency.Milliseconds(),
	}
}

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
	if err != nil {
		return NewUnhealthyReport(err.Error(), latency)
	}
	return NewHealthyReport(latency)
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
