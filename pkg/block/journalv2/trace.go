package journalv2

import "github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"

// The journal's tracer and the operations it traces, declared in internal/core
// because core.Config holds the tracer. Each one's contract is
// documented at its declaration.
type (
	Tracer = core.Tracer
	SpanOp = core.SpanOp
)

// The operations the journal traces. See SpanOp.
const (
	SpanOpen          = core.SpanOpen
	SpanRecoveryPhase = core.SpanRecoveryPhase
	SpanRepackPass    = core.SpanRepackPass
	SpanSeal          = core.SpanSeal
	SpanRetire        = core.SpanRetire
)
