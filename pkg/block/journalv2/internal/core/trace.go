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
// decision: spans take no request context. The journal's operations carry
// none, so a journal span is a root, correlated with the request that caused
// it through its attributes (journal, share tag, file). Parenting journal
// spans under request spans would need context-taking variants of the calls,
// not a change to the existing signatures.
type Tracer func(op SpanOp, attrs ...slog.Attr) (end func(err error))

// SpanOp names the operation a span covers. Its values are the span names
// operators see, so none carries the journal's version. Each is
// journal.<Name>, the journal's prefix from RFC 25's prefix table, as RFC 25
// §6.1 requires of every span name.
type SpanOp string

// The operations the journal traces.
const (
	SpanOpen           SpanOp = "journal.Open"
	SpanRecoveryPhase  SpanOp = "journal.RecoveryPhase"
	SpanSync           SpanOp = "journal.Sync"
	SpanOffloadPass    SpanOp = "journal.OffloadPass"
	SpanRepackPass     SpanOp = "journal.RepackPass"
	SpanFill           SpanOp = "journal.Fill"
	SpanReserveRefused SpanOp = "journal.ReserveRefused"
	SpanSeal           SpanOp = "journal.Seal"
	SpanRetire         SpanOp = "journal.Retire"
)
