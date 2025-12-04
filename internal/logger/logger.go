package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level represents log levels
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Config holds logger configuration
type Config struct {
	Level  string // DEBUG, INFO, WARN, ERROR
	Format string // text, json
	Output string // stdout, stderr, or file path
}

var (
	currentLevel  atomic.Int32
	currentFormat atomic.Value // stores "text" or "json"

	mu       sync.RWMutex
	handler  slog.Handler
	slogger  *slog.Logger
	output   io.Writer = os.Stdout
	useColor bool      = true
)

func init() {
	// Set default level to Info
	currentLevel.Store(int32(LevelInfo))
	currentFormat.Store("text")

	// Check if stdout is a terminal for color support
	if f, ok := output.(*os.File); ok {
		useColor = isTerminal(f.Fd())
	}

	// Initialize default handler
	reconfigure()
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// toSlogLevel converts internal level to slog.Level
func toSlogLevel(l Level) slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// reconfigure rebuilds the slog handler based on current settings
func reconfigure() {
	mu.Lock()
	defer mu.Unlock()

	level := Level(currentLevel.Load())
	format, _ := currentFormat.Load().(string)

	levelVar := new(slog.LevelVar)
	levelVar.Set(toSlogLevel(level))

	opts := &slog.HandlerOptions{
		Level: levelVar,
	}

	if format == "json" {
		handler = slog.NewJSONHandler(output, opts)
	} else {
		handler = NewColorTextHandler(output, opts, useColor)
	}

	slogger = slog.New(handler)
}

// Init initializes the logger with the given configuration.
// Output can be "stdout", "stderr", or a file path.
func Init(cfg Config) error {
	if cfg.Output != "" {
		mu.Lock()
		var newOutput io.Writer
		var newUseColor bool

		switch strings.ToLower(cfg.Output) {
		case "stdout", "":
			newOutput = os.Stdout
			newUseColor = isTerminal(os.Stdout.Fd())
		case "stderr":
			newOutput = os.Stderr
			newUseColor = isTerminal(os.Stderr.Fd())
		default:
			// Assume it's a file path
			f, err := os.OpenFile(cfg.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				mu.Unlock()
				return fmt.Errorf("failed to open log file %q: %w", cfg.Output, err)
			}
			newOutput = f
			newUseColor = false // Files don't support color
		}

		output = newOutput
		useColor = newUseColor
		mu.Unlock()
	}

	if cfg.Level != "" {
		SetLevel(cfg.Level)
	}

	if cfg.Format != "" {
		SetFormat(cfg.Format)
	}

	return nil
}

// InitWithWriter initializes the logger with a custom io.Writer.
// This is primarily useful for testing.
func InitWithWriter(w io.Writer, level, format string, enableColor bool) {
	mu.Lock()
	output = w
	useColor = enableColor
	mu.Unlock()

	if level != "" {
		SetLevel(level)
	}
	if format != "" {
		SetFormat(format)
	}
}

// SetLevel sets the minimum log level
func SetLevel(level string) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		currentLevel.Store(int32(LevelDebug))
	case "INFO":
		currentLevel.Store(int32(LevelInfo))
	case "WARN":
		currentLevel.Store(int32(LevelWarn))
	case "ERROR":
		currentLevel.Store(int32(LevelError))
	default:
		return // ignore invalid levels
	}
	reconfigure()
}

// SetFormat sets the output format (text or json)
func SetFormat(format string) {
	format = strings.ToLower(format)
	if format != "text" && format != "json" {
		return // ignore invalid formats
	}
	currentFormat.Store(format)
	reconfigure()
}

// getLogger returns the current slog logger
func getLogger() *slog.Logger {
	mu.RLock()
	l := slogger
	mu.RUnlock()
	return l
}

// ============================================================================
// Structured Logging API (new primary API)
// ============================================================================

// Debug logs at debug level with structured fields
// Usage: Debug("message", "key1", value1, "key2", value2)
func Debug(msg string, args ...any) {
	if LevelDebug < Level(currentLevel.Load()) {
		return
	}
	getLogger().Debug(msg, args...)
}

// Info logs at info level with structured fields
// Usage: Info("message", "key1", value1, "key2", value2)
func Info(msg string, args ...any) {
	if LevelInfo < Level(currentLevel.Load()) {
		return
	}
	getLogger().Info(msg, args...)
}

// Warn logs at warn level with structured fields
// Usage: Warn("message", "key1", value1, "key2", value2)
func Warn(msg string, args ...any) {
	if LevelWarn < Level(currentLevel.Load()) {
		return
	}
	getLogger().Warn(msg, args...)
}

// Error logs at error level with structured fields
// Usage: Error("message", "key1", value1, "key2", value2)
func Error(msg string, args ...any) {
	getLogger().Error(msg, args...)
}

// ============================================================================
// Context-aware Logging API
// ============================================================================

// DebugCtx logs at debug level with context (auto-injects trace_id, span_id, etc.)
func DebugCtx(ctx context.Context, msg string, args ...any) {
	if LevelDebug < Level(currentLevel.Load()) {
		return
	}
	args = appendContextFields(ctx, args)
	getLogger().Debug(msg, args...)
}

// InfoCtx logs at info level with context
func InfoCtx(ctx context.Context, msg string, args ...any) {
	if LevelInfo < Level(currentLevel.Load()) {
		return
	}
	args = appendContextFields(ctx, args)
	getLogger().Info(msg, args...)
}

// WarnCtx logs at warn level with context
func WarnCtx(ctx context.Context, msg string, args ...any) {
	if LevelWarn < Level(currentLevel.Load()) {
		return
	}
	args = appendContextFields(ctx, args)
	getLogger().Warn(msg, args...)
}

// ErrorCtx logs at error level with context
func ErrorCtx(ctx context.Context, msg string, args ...any) {
	args = appendContextFields(ctx, args)
	getLogger().Error(msg, args...)
}

// appendContextFields adds LogContext fields to args
func appendContextFields(ctx context.Context, args []any) []any {
	lc := FromContext(ctx)
	if lc == nil {
		return args
	}

	// Prepend context fields so they appear first in output
	ctxArgs := make([]any, 0, 14+len(args))

	if lc.TraceID != "" {
		ctxArgs = append(ctxArgs, KeyTraceID, lc.TraceID)
	}
	if lc.SpanID != "" {
		ctxArgs = append(ctxArgs, KeySpanID, lc.SpanID)
	}
	if lc.Procedure != "" {
		ctxArgs = append(ctxArgs, KeyProcedure, lc.Procedure)
	}
	if lc.Share != "" {
		ctxArgs = append(ctxArgs, KeyShare, lc.Share)
	}
	if lc.ClientIP != "" {
		ctxArgs = append(ctxArgs, KeyClientIP, lc.ClientIP)
	}
	if lc.UID != 0 {
		ctxArgs = append(ctxArgs, KeyUID, lc.UID)
	}
	if lc.GID != 0 {
		ctxArgs = append(ctxArgs, KeyGID, lc.GID)
	}

	ctxArgs = append(ctxArgs, args...)
	return ctxArgs
}

// ============================================================================
// Logger with pre-bound fields
// ============================================================================

// With returns a new slog.Logger with additional attributes
func With(args ...any) *slog.Logger {
	return getLogger().With(args...)
}

// ============================================================================
// Duration helper
// ============================================================================

// Duration returns duration since start time in milliseconds
func Duration(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

// Debugf logs at debug level with printf-style formatting (backward compatibility)
func Debugf(format string, v ...any) {
	if LevelDebug < Level(currentLevel.Load()) {
		return
	}
	msg := fmt.Sprintf(format, v...)
	getLogger().Debug(msg)
}

// Infof logs at info level with printf-style formatting (backward compatibility)
func Infof(format string, v ...any) {
	if LevelInfo < Level(currentLevel.Load()) {
		return
	}
	msg := fmt.Sprintf(format, v...)
	getLogger().Info(msg)
}

// Warnf logs at warn level with printf-style formatting (backward compatibility)
func Warnf(format string, v ...any) {
	if LevelWarn < Level(currentLevel.Load()) {
		return
	}
	msg := fmt.Sprintf(format, v...)
	getLogger().Warn(msg)
}

// Errorf logs at error level with printf-style formatting (backward compatibility)
func Errorf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	getLogger().Error(msg)
}
