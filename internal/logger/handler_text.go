package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

// ColorTextHandler implements slog.Handler with colored text output
type ColorTextHandler struct {
	opts     *slog.HandlerOptions
	w        io.Writer
	mu       *sync.Mutex
	attrs    []slog.Attr
	groups   []string
	useColor bool
}

// NewColorTextHandler creates a new ColorTextHandler
func NewColorTextHandler(w io.Writer, opts *slog.HandlerOptions, useColor bool) *ColorTextHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}

	return &ColorTextHandler{
		opts:     opts,
		w:        w,
		mu:       &sync.Mutex{},
		useColor: useColor,
	}
}

// Enabled reports whether the handler handles records at the given level
func (h *ColorTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return level >= minLevel
}

// Handle formats and writes a log record
func (h *ColorTextHandler) Handle(_ context.Context, r slog.Record) error {
	// Format timestamp
	timestamp := r.Time.Format("2006-01-02 15:04:05")

	// Format level with color
	levelStr := h.formatLevel(r.Level)

	// Build output (not under lock - local buffer)
	var buf []byte
	buf = fmt.Appendf(buf, "[%s] [%s] %s", timestamp, levelStr, r.Message)

	// Add pre-defined attrs from handler
	for _, attr := range h.attrs {
		buf = h.appendAttr(buf, attr)
	}

	// Add record attrs
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, a)
		return true
	})

	buf = append(buf, '\n')

	// Only lock for the actual write
	h.mu.Lock()
	_, err := h.w.Write(buf)
	h.mu.Unlock()
	return err
}

// formatLevel returns the level string with optional color
func (h *ColorTextHandler) formatLevel(level slog.Level) string {
	var levelStr string
	var color string

	switch {
	case level < slog.LevelInfo:
		levelStr = "DEBUG"
		color = colorGray
	case level < slog.LevelWarn:
		levelStr = "INFO"
		color = colorGreen
	case level < slog.LevelError:
		levelStr = "WARN"
		color = colorYellow
	default:
		levelStr = "ERROR"
		color = colorRed
	}

	if h.useColor {
		return fmt.Sprintf("%s%s%s", color, levelStr, colorReset)
	}
	return levelStr
}

// appendAttr formats and appends an attribute
func (h *ColorTextHandler) appendAttr(buf []byte, a slog.Attr) []byte {
	if a.Equal(slog.Attr{}) {
		return buf
	}

	// Resolve the attribute value
	a.Value = a.Value.Resolve()

	key := a.Key
	val := formatValue(a.Value)

	if h.useColor {
		buf = fmt.Appendf(buf, " %s%s%s=%s", colorCyan, key, colorReset, val)
	} else {
		buf = fmt.Appendf(buf, " %s=%s", key, val)
	}

	return buf
}

// formatValue formats a slog.Value for text output
func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return fmt.Sprintf("%d", v.Int64())
	case slog.KindUint64:
		return fmt.Sprintf("%d", v.Uint64())
	case slog.KindFloat64:
		return fmt.Sprintf("%.3f", v.Float64())
	case slog.KindBool:
		return fmt.Sprintf("%t", v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindAny:
		return fmt.Sprintf("%v", v.Any())
	default:
		return v.String()
	}
}

// WithAttrs returns a new handler with additional attrs
func (h *ColorTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandler := &ColorTextHandler{
		opts:     h.opts,
		w:        h.w,
		mu:       h.mu, // Share mutex with parent
		attrs:    append(append([]slog.Attr{}, h.attrs...), attrs...),
		groups:   append([]string{}, h.groups...),
		useColor: h.useColor,
	}
	return newHandler
}

// WithGroup returns a new handler with a group name
func (h *ColorTextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	newHandler := &ColorTextHandler{
		opts:     h.opts,
		w:        h.w,
		mu:       h.mu,
		attrs:    append([]slog.Attr{}, h.attrs...),
		groups:   append(append([]string{}, h.groups...), name),
		useColor: h.useColor,
	}
	return newHandler
}
