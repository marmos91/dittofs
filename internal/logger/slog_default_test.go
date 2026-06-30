package logger

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Verifies that std-lib slog.* (used across pkg/block, incl. the GC engine)
// honors DITTOFS_LOGGING_LEVEL via slog.SetDefault in reconfigure().
func TestStdlibSlogHonorsConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	InitWithWriter(&buf, "DEBUG", "text", false)
	slog.Debug("engine-style-debug-line", "k", "v")
	if !strings.Contains(buf.String(), "engine-style-debug-line") {
		t.Fatalf("std slog.Debug did not route through configured DEBUG handler; got: %q", buf.String())
	}

	buf.Reset()
	InitWithWriter(&buf, "INFO", "text", false)
	slog.Debug("should-be-suppressed")
	if strings.Contains(buf.String(), "should-be-suppressed") {
		t.Fatalf("std slog.Debug leaked at INFO level; got: %q", buf.String())
	}
}
