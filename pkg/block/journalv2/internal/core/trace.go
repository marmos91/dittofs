package core

import "log/slog"

// Tracer starts a span for op, with attrs, and returns the function that ends
// it with the operation's error, nil when it succeeded. A journal opened
// without a tracer traces nothing. A tracer may return a nil end, which the
// journal does not call.
//
// The journal calls a tracer, and the end function it returns, with no
// journal lock held. Neither may block, and neither may call back into the
// journal.
//
// decision: the journal traces only its own background work, as root spans:
// opening and its recovery, sealing an idle segment, retiring a segment, and a
// repack pass. RFC 25 §6.1 makes background work a root span, and the
// journal's operations take no request context, so it could not parent a span
// under a request anyway. A wait on the request path (a sync, a fill, an
// offload) is the engine's: RFC 8 opens a child of the request's span for
// each stage it waits on, and an offload pass as a root of its own, so a
// journal span there would count the same wait twice (RFC 25 §2.7, one event,
// one owner).
type Tracer func(op SpanOp, attrs ...slog.Attr) (end func(err error))

// SpanOp names the operation a span covers. Its values are the span names
// operators see, so none carries the journal's version. Each is
// journal.<Name>, the journal's prefix from RFC 25's prefix table, as RFC 25
// §6.1 requires of every span name.
type SpanOp string

// The operations the journal traces: its background work only (see Tracer).
const (
	SpanOpen          SpanOp = "journal.Open"
	SpanRecoveryPhase SpanOp = "journal.RecoveryPhase"
	SpanRepackPass    SpanOp = "journal.RepackPass"
	SpanSeal          SpanOp = "journal.Seal"
	SpanRetire        SpanOp = "journal.Retire"
)
