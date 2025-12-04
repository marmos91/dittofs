package logger

import (
	"context"
	"time"
)

// contextKey is a private type for context keys to avoid collisions
type contextKey struct{}

// logContextKey is the key for LogContext in context.Context
var logContextKey = contextKey{}

// LogContext holds request-scoped logging context
type LogContext struct {
	TraceID    string    // OpenTelemetry trace ID
	SpanID     string    // OpenTelemetry span ID
	Procedure  string    // NFS procedure name (READ, WRITE, LOOKUP, etc.)
	Share      string    // Share path (/export, etc.)
	ClientIP   string    // Client IP address (without port)
	UID        uint32    // Effective user ID
	GID        uint32    // Effective group ID
	AuthFlavor uint32    // RPC auth flavor
	StartTime  time.Time // For duration calculation
}

// WithContext returns a new context with the given LogContext
func WithContext(ctx context.Context, lc *LogContext) context.Context {
	return context.WithValue(ctx, logContextKey, lc)
}

// FromContext retrieves the LogContext from context, or nil if not present
func FromContext(ctx context.Context) *LogContext {
	if ctx == nil {
		return nil
	}
	lc, _ := ctx.Value(logContextKey).(*LogContext)
	return lc
}

// NewLogContext creates a new LogContext with the given client IP
func NewLogContext(clientIP string) *LogContext {
	return &LogContext{
		ClientIP:  clientIP,
		StartTime: time.Now(),
	}
}

// Clone creates a copy of the LogContext
func (lc *LogContext) Clone() *LogContext {
	if lc == nil {
		return nil
	}
	return &LogContext{
		TraceID:    lc.TraceID,
		SpanID:     lc.SpanID,
		Procedure:  lc.Procedure,
		Share:      lc.Share,
		ClientIP:   lc.ClientIP,
		UID:        lc.UID,
		GID:        lc.GID,
		AuthFlavor: lc.AuthFlavor,
		StartTime:  lc.StartTime,
	}
}

// WithProcedure returns a copy with the procedure set
func (lc *LogContext) WithProcedure(procedure string) *LogContext {
	clone := lc.Clone()
	if clone != nil {
		clone.Procedure = procedure
	}
	return clone
}

// WithShare returns a copy with the share set
func (lc *LogContext) WithShare(share string) *LogContext {
	clone := lc.Clone()
	if clone != nil {
		clone.Share = share
	}
	return clone
}

// WithAuth returns a copy with authentication info set
func (lc *LogContext) WithAuth(uid, gid, authFlavor uint32) *LogContext {
	clone := lc.Clone()
	if clone != nil {
		clone.UID = uid
		clone.GID = gid
		clone.AuthFlavor = authFlavor
	}
	return clone
}

// WithTrace returns a copy with trace info set
func (lc *LogContext) WithTrace(traceID, spanID string) *LogContext {
	clone := lc.Clone()
	if clone != nil {
		clone.TraceID = traceID
		clone.SpanID = spanID
	}
	return clone
}

// DurationMs returns the duration since StartTime in milliseconds
func (lc *LogContext) DurationMs() float64 {
	if lc == nil || lc.StartTime.IsZero() {
		return 0
	}
	return float64(time.Since(lc.StartTime).Microseconds()) / 1000.0
}
