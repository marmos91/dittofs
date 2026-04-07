package health

import (
	"context"
	"errors"
	"time"
)

// NewHealthyReport returns a [StatusHealthy] [Report] stamped with the
// current UTC time and the supplied probe latency. Use this from store
// implementations whose Healthcheck method has measured wall-clock time
// around its probe and just needs to package the result.
//
// The CheckedAt timestamp is the moment this helper is called (probe
// completion time), not a value supplied by the caller. The latency
// argument is the only externally measured value.
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

// NewUnknownReport returns a [StatusUnknown] [Report] with the supplied
// message and latency. Use this when the probe could not produce a
// definitive answer — most commonly because the caller's context was
// canceled or its deadline expired before the probe could complete.
//
// Per the [Checker] contract, context-related termination must surface
// as [StatusUnknown] (the probe was indeterminate) rather than
// [StatusUnhealthy] (which would falsely suggest the entity is broken).
func NewUnknownReport(msg string, latency time.Duration) Report {
	return Report{
		Status:    StatusUnknown,
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
// Mapping rules:
//
//   - nil error → [StatusHealthy] with empty message.
//   - [context.Canceled] or [context.DeadlineExceeded] (matched via
//     [errors.Is], so wrapped variants count) → [StatusUnknown]. The
//     probe was aborted by the caller, not by the entity being checked.
//   - any other non-nil error → [StatusUnhealthy] with the error
//     string as the message.
//
// CheckedAt is set to the current UTC time when the helper is called
// (probe completion). LatencyMs is the supplied wall-clock duration —
// the caller is responsible for measuring it around the probe call.
//
// For more nuanced status derivation (e.g. mapping a specific sentinel
// error to [StatusDegraded]), build the [Report] manually instead.
func ReportFromError(err error, latency time.Duration) Report {
	if err == nil {
		return NewHealthyReport(latency)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewUnknownReport(err.Error(), latency)
	}
	return NewUnhealthyReport(err.Error(), latency)
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
