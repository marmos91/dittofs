package core

import (
	"context"
	"log/slog"
)

// Tracer starts a span for op, with attrs, as a child of the span ctx carries,
// and returns the context carrying the new span with the function that ends it
// with the operation's error, nil when it succeeded. The journal runs the
// operation's nested work under the returned context, so a span the journal
// opens inside it is that span's child. A journal opened without a tracer
// traces nothing. A tracer that records nothing returns ctx and a nil end,
// which the journal does not call.
//
// The journal calls a tracer, and the end function it returns, with no
// journal lock held. Neither may block, and neither may call back into the
// journal.
//
// decision: a journal span is a child of the caller's span, carried by the
// context of the call that started the work. Work the journal starts on its
// own, with no caller, runs under a background context, so its span is a root,
// as RFC 25 §6.1 makes background work.
type Tracer func(ctx context.Context, op SpanOp, attrs ...slog.Attr) (context.Context, func(err error))

// SpanOp names the operation a span covers. Its values are the span names
// operators see, so none carries the journal's version. Each is
// journal.<Name>, the journal's prefix from RFC 25's prefix table, as RFC 25
// §6.1 requires of every span name.
//
// decision: no span op is declared before the code that emits it. A span name
// is what traces and dashboards key on once a release emits it, so a name is
// added with its emitter rather than declared ahead and pruned later.
type SpanOp string
