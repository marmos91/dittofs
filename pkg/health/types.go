// Package health defines the common health-reporting types used by stores,
// adapters, and shares. Every entity that can be in a "good" or "bad" state
// implements [Checker], and every API response that wants to carry health
// information embeds a [Report].
//
// This package is intentionally small and free of dependencies on any other
// dittofs package, so every consumer can import it without cycle risk.
package health

import (
	"context"
	"time"
)

// Status is the health state of a single entity. The set is deliberately
// small — consumers should be able to render a five-way switch and no more.
// When adding a value, update every consumer's switch statement and any
// validation logic that may be introduced in the future.
type Status string

const (
	// StatusHealthy means the entity is working as expected.
	StatusHealthy Status = "healthy"

	// StatusDegraded means the entity is still serving requests but
	// something is off (high latency, transient errors, reduced capacity).
	// Operators should investigate but no immediate action is required.
	StatusDegraded Status = "degraded"

	// StatusUnhealthy means the entity is broken: read/write is failing,
	// the process has crashed, or the backing resource is unreachable.
	StatusUnhealthy Status = "unhealthy"

	// StatusUnknown means the status hasn't been computed yet, or the
	// underlying check couldn't produce a definitive answer (e.g. the
	// probe timed out). Prefer explicit [StatusDegraded] or
	// [StatusUnhealthy] when the failure mode is known.
	StatusUnknown Status = "unknown"

	// StatusDisabled is specific to entities that can be turned off by
	// configuration (currently: adapters). A disabled adapter is neither
	// healthy nor unhealthy — it is not running on purpose.
	StatusDisabled Status = "disabled"
)

// String returns the canonical string form of the status, which matches the
// JSON wire value. Implementing Stringer makes the type play nicely with
// fmt and structured logging.
func (s Status) String() string { return string(s) }

// Report is a point-in-time health snapshot for a single entity. It is
// produced by [Checker.Healthcheck] and typically embedded in API
// responses alongside the entity itself.
//
// The Message field is free-form and intended for operator display ("S3
// endpoint returned 503 on last probe"). It should not be parsed by
// callers — use Status for programmatic decisions.
type Report struct {
	// Status is the categorical health state. Required; never empty.
	Status Status `json:"status"`

	// Message is an optional human-readable explanation. For healthy
	// reports it is usually empty; for non-healthy reports it should
	// describe the observed failure so an operator can act on it.
	Message string `json:"message,omitempty"`

	// CheckedAt is the timestamp at which the underlying probe ran.
	// Implementations should produce UTC values (time.Now().UTC()), but
	// the type itself cannot enforce this — consumers that need UTC must
	// call .UTC() defensively. Cache wrappers preserve the original probe
	// timestamp so clients can tell how stale the data is.
	CheckedAt time.Time `json:"checked_at"`

	// LatencyMs is the wall-clock duration of the probe in milliseconds.
	// Useful for surfacing "degraded due to latency" without requiring
	// callers to parse Message. Zero if not measured.
	LatencyMs int64 `json:"latency_ms,omitempty"`
}

// Checker is implemented by anything that can report its own health:
// stores, adapters, shares, and wrappers around them. Implementations must
// be safe for concurrent use — the API layer will often call Healthcheck
// from multiple goroutines at once.
//
// Healthcheck must respect the caller-supplied context: if the context is
// canceled or deadlines out, the check should abort and return an
// [StatusUnknown] report with a context-related message rather than
// blocking indefinitely.
type Checker interface {
	// Healthcheck runs the health probe and returns a fresh Report.
	// Implementations should set Report.CheckedAt to the actual probe
	// time and Report.LatencyMs to the measured duration.
	Healthcheck(ctx context.Context) Report
}

// CheckerFunc adapts an ordinary function to the [Checker] interface,
// matching the standard library pattern (http.HandlerFunc etc.). Useful for
// test fakes and for wrapping ad-hoc probes without defining a new type.
type CheckerFunc func(ctx context.Context) Report

// Healthcheck calls f(ctx).
func (f CheckerFunc) Healthcheck(ctx context.Context) Report { return f(ctx) }

// now is the clock used by cache TTL logic. It is overridable in tests by
// test files in this package so the cache's time-based behavior can be
// driven by a fake clock without sleeping. Keeping it unexported prevents
// external packages from reassigning it and introducing hard-to-debug
// timing issues or races.
var now = time.Now
