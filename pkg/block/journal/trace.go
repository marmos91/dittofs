package journal

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// The journal's tracer and the operations it traces, declared in internal/core
// because core.Config holds the tracer. Each one's contract is
// documented at its declaration.
type (
	Tracer = core.Tracer
	SpanOp = core.SpanOp
)
