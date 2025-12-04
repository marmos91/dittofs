package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Helper Functions
// ============================================================================

// captureOutput redirects logger output to a buffer for testing.
// Returns the buffer and a cleanup function to restore original output.
func captureOutput() (*bytes.Buffer, func()) {
	buf := new(bytes.Buffer)

	mu.Lock()
	originalOutput := output
	originalColor := useColor
	output = buf
	useColor = false // Disable colors for easier testing
	mu.Unlock()

	// Reconfigure with new output
	reconfigure()

	cleanup := func() {
		mu.Lock()
		output = originalOutput
		useColor = originalColor
		mu.Unlock()
		reconfigure()
	}

	return buf, cleanup
}

// ============================================================================
// Level Filtering Tests
// ============================================================================

func TestLevelFiltering(t *testing.T) {
	t.Run("DebugLevelShowsAllMessages", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("DEBUG")

		Debug("debug message")
		Info("info message")
		Warn("warn message")
		Error("error message")

		output := buf.String()
		assert.Contains(t, output, "DEBUG")
		assert.Contains(t, output, "INFO")
		assert.Contains(t, output, "WARN")
		assert.Contains(t, output, "ERROR")
		assert.Contains(t, output, "debug message")
		assert.Contains(t, output, "info message")
		assert.Contains(t, output, "warn message")
		assert.Contains(t, output, "error message")
	})

	t.Run("InfoLevelFiltersDebug", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")

		Debug("debug message")
		Info("info message")
		Warn("warn message")
		Error("error message")

		output := buf.String()
		assert.NotContains(t, output, "DEBUG")
		assert.NotContains(t, output, "debug message")
		assert.Contains(t, output, "INFO")
		assert.Contains(t, output, "WARN")
		assert.Contains(t, output, "ERROR")
	})

	t.Run("WarnLevelFiltersDebugAndInfo", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("WARN")

		Debug("debug message")
		Info("info message")
		Warn("warn message")
		Error("error message")

		output := buf.String()
		assert.NotContains(t, output, "DEBUG")
		assert.NotContains(t, output, "INFO")
		assert.Contains(t, output, "WARN")
		assert.Contains(t, output, "ERROR")
	})

	t.Run("ErrorLevelShowsOnlyErrors", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("ERROR")

		Debug("debug message")
		Info("info message")
		Warn("warn message")
		Error("error message")

		output := buf.String()
		assert.NotContains(t, output, "DEBUG")
		assert.NotContains(t, output, "INFO")
		assert.NotContains(t, output, "WARN")
		assert.Contains(t, output, "ERROR")
		assert.Contains(t, output, "error message")
	})
}

// ============================================================================
// SetLevel Tests
// ============================================================================

func TestSetLevel(t *testing.T) {
	t.Run("SetLevelChangesFilteringBehavior", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		// Start at ERROR level
		SetLevel("ERROR")
		Info("should not appear")
		buf.Reset()

		// Change to INFO level
		SetLevel("INFO")
		Info("should appear")

		output := buf.String()
		assert.Contains(t, output, "should appear")
		assert.NotContains(t, output, "should not appear")
	})

	t.Run("SetLevelIsCaseInsensitive", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("debug")
		Debug("test message")
		assert.Contains(t, buf.String(), "test message")

		buf.Reset()
		SetLevel("DeBuG")
		Debug("test message 2")
		assert.Contains(t, buf.String(), "test message 2")
	})

	t.Run("SetLevelIgnoresInvalidValues", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		// Set to INFO
		SetLevel("INFO")
		Info("info message")
		output1 := buf.String()
		assert.Contains(t, output1, "INFO")
		buf.Reset()

		// Try to set invalid level - should stay at INFO
		SetLevel("INVALID")
		Debug("debug message")
		Info("info message 2")

		output2 := buf.String()
		// Should still be at INFO level (debug filtered, info shown)
		assert.NotContains(t, output2, "debug message")
		assert.Contains(t, output2, "info message 2")
	})
}

// ============================================================================
// Message Formatting Tests
// ============================================================================

func TestMessageFormatting(t *testing.T) {
	t.Run("FormatsMessagesWithTimestamp", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Info("test message")

		output := buf.String()
		// Should contain timestamp format YYYY-MM-DD HH:MM:SS
		assert.Regexp(t, `\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\]`, output)
	})

	t.Run("FormatsMessagesWithLevel", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("DEBUG")

		Debug("test")
		Info("test")
		Warn("test")
		Error("test")

		output := buf.String()
		assert.Contains(t, output, "[DEBUG]")
		assert.Contains(t, output, "[INFO]")
		assert.Contains(t, output, "[WARN]")
		assert.Contains(t, output, "[ERROR]")
	})

	t.Run("FormatsMessagesWithStructuredFields", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Info("user logged in", "username", "alice", "user_id", 42)

		output := buf.String()
		assert.Contains(t, output, "user logged in")
		assert.Contains(t, output, "username=alice")
		assert.Contains(t, output, "user_id=42")
	})

	t.Run("HandlesEmptyMessages", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Info("")

		output := buf.String()
		// Should still have timestamp and level even with empty message
		assert.Contains(t, output, "[INFO]")
	})

	t.Run("HandlesMultilineMessages", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Info("line1\nline2\nline3")

		output := buf.String()
		assert.Contains(t, output, "line1")
		assert.Contains(t, output, "line2")
		assert.Contains(t, output, "line3")
	})
}

// ============================================================================
// Level String Tests
// ============================================================================

func TestLevelString(t *testing.T) {
	t.Run("LevelDebugToString", func(t *testing.T) {
		assert.Equal(t, "DEBUG", LevelDebug.String())
	})

	t.Run("LevelInfoToString", func(t *testing.T) {
		assert.Equal(t, "INFO", LevelInfo.String())
	})

	t.Run("LevelWarnToString", func(t *testing.T) {
		assert.Equal(t, "WARN", LevelWarn.String())
	})

	t.Run("LevelErrorToString", func(t *testing.T) {
		assert.Equal(t, "ERROR", LevelError.String())
	})

	t.Run("InvalidLevelToString", func(t *testing.T) {
		invalidLevel := Level(99)
		assert.Equal(t, "UNKNOWN", invalidLevel.String())
	})
}

// ============================================================================
// Concurrency Tests
// ============================================================================

func TestConcurrentLogging(t *testing.T) {
	t.Run("ConcurrentLogsDoNotRace", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")

		const numGoroutines = 10
		const logsPerGoroutine = 100

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(id int) {
				defer wg.Done()
				for j := 0; j < logsPerGoroutine; j++ {
					Info("goroutine log", "id", id, "iteration", j)
				}
			}(i)
		}

		wg.Wait()

		output := buf.String()
		lines := strings.Split(strings.TrimSpace(output), "\n")

		// Should have exactly numGoroutines * logsPerGoroutine lines
		assert.Equal(t, numGoroutines*logsPerGoroutine, len(lines))
	})

	t.Run("ConcurrentLevelChanges", func(t *testing.T) {
		// Use io.Discard for this test since changing levels reconfigures the logger
		// which creates new handlers, and bytes.Buffer is not thread-safe
		InitWithWriter(io.Discard, "DEBUG", "text", false)
		defer func() {
			// Reset to default after test
			mu.Lock()
			output = os.Stdout
			mu.Unlock()
			reconfigure()
		}()

		const numGoroutines = 5
		const iterations = 50

		var wg sync.WaitGroup
		wg.Add(numGoroutines * 2)

		// Goroutines that change levels
		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < iterations; j++ {
					if j%2 == 0 {
						SetLevel("DEBUG")
					} else {
						SetLevel("ERROR")
					}
				}
			}()
		}

		// Goroutines that log
		for i := 0; i < numGoroutines; i++ {
			go func(id int) {
				defer wg.Done()
				for j := 0; j < iterations; j++ {
					Debug("debug", "id", id)
					Info("info", "id", id)
					Warn("warn", "id", id)
					Error("error", "id", id)
				}
			}(i)
		}

		// Should not panic or race
		require.NotPanics(t, func() {
			wg.Wait()
		})
	})
}

// ============================================================================
// Default Behavior Tests
// ============================================================================

func TestDefaultBehavior(t *testing.T) {
	t.Run("DefaultLevelIsInfo", func(t *testing.T) {
		// Reset to default by calling init behavior
		currentLevel.Store(int32(LevelInfo))

		buf, cleanup := captureOutput()
		defer cleanup()

		Debug("should not appear")
		Info("should appear")

		output := buf.String()
		assert.NotContains(t, output, "should not appear")
		assert.Contains(t, output, "should appear")
	})
}

// ============================================================================
// JSON Format Tests
// ============================================================================

func TestJSONFormat(t *testing.T) {
	t.Run("JSONFormatProducesValidJSON", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		SetFormat("json")

		Info("test message", "key1", "value1", "key2", 42)

		output := strings.TrimSpace(buf.String())

		// Verify it's valid JSON
		var entry map[string]any
		err := json.Unmarshal([]byte(output), &entry)
		require.NoError(t, err, "Output should be valid JSON: %s", output)

		assert.Equal(t, "INFO", entry["level"])
		assert.Equal(t, "test message", entry["msg"])
		assert.Equal(t, "value1", entry["key1"])
		assert.Equal(t, float64(42), entry["key2"]) // JSON numbers are float64
	})

	t.Run("JSONFormatIncludesTimestamp", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		SetFormat("json")

		Info("test message")

		var entry map[string]any
		err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry)
		require.NoError(t, err)

		assert.Contains(t, entry, "time")
	})
}

// ============================================================================
// Format Switching Tests
// ============================================================================

func TestFormatSwitching(t *testing.T) {
	t.Run("SwitchFromTextToJSON", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")

		// Start with text
		SetFormat("text")
		Info("text message")
		textOutput := buf.String()
		buf.Reset()

		// Switch to JSON
		SetFormat("json")
		Info("json message")
		jsonOutput := strings.TrimSpace(buf.String())

		// Verify different formats
		assert.Contains(t, textOutput, "[INFO]")
		assert.True(t, json.Valid([]byte(jsonOutput)), "Should be valid JSON")
	})

	t.Run("InvalidFormatIgnored", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		SetFormat("text")

		// Try invalid format
		SetFormat("xml")

		Info("test message")

		// Should still be text format
		output := buf.String()
		assert.Contains(t, output, "[INFO]")
	})
}

// ============================================================================
// Context Logging Tests
// ============================================================================

func TestContextLogging(t *testing.T) {
	t.Run("LogContextInjectsFields", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		SetFormat("json")

		lc := &LogContext{
			TraceID:   "abc123",
			SpanID:    "xyz789",
			Procedure: "READ",
			Share:     "/export",
			ClientIP:  "192.168.1.100",
			UID:       1000,
			GID:       1000,
		}
		ctx := WithContext(context.Background(), lc)

		InfoCtx(ctx, "operation completed", "extra_field", "value")

		var entry map[string]any
		err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry)
		require.NoError(t, err)

		assert.Equal(t, "abc123", entry["trace_id"])
		assert.Equal(t, "xyz789", entry["span_id"])
		assert.Equal(t, "READ", entry["procedure"])
		assert.Equal(t, "/export", entry["share"])
		assert.Equal(t, "192.168.1.100", entry["client_ip"])
		assert.Equal(t, float64(1000), entry["uid"])
		assert.Equal(t, float64(1000), entry["gid"])
		assert.Equal(t, "value", entry["extra_field"])
	})

	t.Run("NilContextHandled", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")

		// Should not panic with nil context
		require.NotPanics(t, func() {
			InfoCtx(nil, "test message")
		})

		assert.Contains(t, buf.String(), "test message")
	})

	t.Run("ContextWithoutLogContextHandled", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")

		// Should work with context that has no LogContext
		require.NotPanics(t, func() {
			InfoCtx(context.Background(), "test message")
		})

		assert.Contains(t, buf.String(), "test message")
	})
}

// ============================================================================
// LogContext Tests
// ============================================================================

func TestLogContext(t *testing.T) {
	t.Run("NewLogContext", func(t *testing.T) {
		lc := NewLogContext("192.168.1.100")
		assert.Equal(t, "192.168.1.100", lc.ClientIP)
		assert.False(t, lc.StartTime.IsZero())
	})

	t.Run("Clone", func(t *testing.T) {
		lc := &LogContext{
			TraceID:   "trace123",
			Procedure: "READ",
			ClientIP:  "192.168.1.100",
			UID:       1000,
		}

		clone := lc.Clone()
		assert.Equal(t, lc.TraceID, clone.TraceID)
		assert.Equal(t, lc.Procedure, clone.Procedure)
		assert.Equal(t, lc.ClientIP, clone.ClientIP)
		assert.Equal(t, lc.UID, clone.UID)

		// Verify it's a different object
		clone.Procedure = "WRITE"
		assert.Equal(t, "READ", lc.Procedure)
	})

	t.Run("CloneNil", func(t *testing.T) {
		var lc *LogContext
		clone := lc.Clone()
		assert.Nil(t, clone)
	})

	t.Run("WithProcedure", func(t *testing.T) {
		lc := NewLogContext("192.168.1.100")
		lc2 := lc.WithProcedure("READ")

		assert.Equal(t, "READ", lc2.Procedure)
		assert.Equal(t, "", lc.Procedure) // Original unchanged
	})

	t.Run("WithAuth", func(t *testing.T) {
		lc := NewLogContext("192.168.1.100")
		lc2 := lc.WithAuth(1000, 1000, 1)

		assert.Equal(t, uint32(1000), lc2.UID)
		assert.Equal(t, uint32(1000), lc2.GID)
		assert.Equal(t, uint32(1), lc2.AuthFlavor)
	})
}

// ============================================================================
// Field Helper Tests
// ============================================================================

func TestFieldHelpers(t *testing.T) {
	t.Run("HandleFormatsAsHex", func(t *testing.T) {
		attr := Handle([]byte{0x01, 0x02, 0x03, 0x04})
		assert.Equal(t, KeyHandle, attr.Key)
		assert.Equal(t, "01020304", attr.Value.String())
	})

	t.Run("ErrHandlesNil", func(t *testing.T) {
		attr := Err(nil)
		assert.Equal(t, "", attr.Key) // Empty attr for nil error
	})

	t.Run("ErrFormatsError", func(t *testing.T) {
		attr := Err(assert.AnError)
		assert.Equal(t, KeyError, attr.Key)
		assert.Contains(t, attr.Value.String(), "assert.AnError")
	})
}

// ============================================================================
// Printf-style Backward Compatibility Tests
// ============================================================================

func TestPrintfStyleLogging(t *testing.T) {
	t.Run("DebugfFormatsCorrectly", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("DEBUG")
		Debugf("user %s has ID %d", "alice", 42)

		assert.Contains(t, buf.String(), "user alice has ID 42")
	})

	t.Run("InfofFormatsCorrectly", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Infof("count: %d", 100)

		assert.Contains(t, buf.String(), "count: 100")
	})

	t.Run("WarnfFormatsCorrectly", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("WARN")
		Warnf("warning: %s", "something happened")

		assert.Contains(t, buf.String(), "warning: something happened")
	})

	t.Run("ErrorfFormatsCorrectly", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("ERROR")
		Errorf("error: %v", "test error")

		assert.Contains(t, buf.String(), "error: test error")
	})
}

// ============================================================================
// Edge Cases Tests
// ============================================================================

func TestEdgeCases(t *testing.T) {
	t.Run("LogWithNoFields", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		require.NotPanics(t, func() {
			Info("test")
		})

		assert.Contains(t, buf.String(), "test")
	})

	t.Run("LogWithSpecialCharacters", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Info("test message", "key", "value with spaces", "key2", "value=with=equals")

		output := buf.String()
		assert.Contains(t, output, "value with spaces")
		assert.Contains(t, output, "value=with=equals")
	})

	t.Run("DurationCalculation", func(t *testing.T) {
		lc := NewLogContext("192.168.1.100")
		// Duration should be positive (non-zero)
		duration := lc.DurationMs()
		assert.GreaterOrEqual(t, duration, 0.0)
	})
}

// ============================================================================
// Init Tests
// ============================================================================

func TestInit(t *testing.T) {
	t.Run("InitWithWriter", func(t *testing.T) {
		buf := new(bytes.Buffer)

		InitWithWriter(buf, "DEBUG", "text", false)

		Debug("test message")
		assert.Contains(t, buf.String(), "test message")

		// Cleanup
		mu.Lock()
		output = os.Stdout
		mu.Unlock()
		reconfigure()
	})

	t.Run("InitWithConfig", func(t *testing.T) {
		// Test with stdout (default)
		err := Init(Config{
			Level:  "DEBUG",
			Format: "text",
			Output: "stdout",
		})
		require.NoError(t, err)

		// Cleanup
		mu.Lock()
		output = os.Stdout
		mu.Unlock()
		reconfigure()
	})

	t.Run("InitWithEmptyConfig", func(t *testing.T) {
		err := Init(Config{})
		require.NoError(t, err)
	})
}

// ============================================================================
// Benchmark Tests
// ============================================================================

func BenchmarkLogDisabled(b *testing.B) {
	buf := new(bytes.Buffer)
	InitWithWriter(buf, "ERROR", "text", false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Debug("test message", "key", "value")
	}
}

func BenchmarkLogText(b *testing.B) {
	buf := new(bytes.Buffer)
	InitWithWriter(buf, "DEBUG", "text", false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Info("test message", "key", "value", "count", i)
	}
}

func BenchmarkLogJSON(b *testing.B) {
	buf := new(bytes.Buffer)
	InitWithWriter(buf, "DEBUG", "json", false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Info("test message", "key", "value", "count", i)
	}
}

func BenchmarkLogCtx(b *testing.B) {
	buf := new(bytes.Buffer)
	InitWithWriter(buf, "DEBUG", "json", false)

	lc := &LogContext{
		TraceID:   "abc123",
		SpanID:    "xyz789",
		Procedure: "READ",
		ClientIP:  "192.168.1.100",
		UID:       1000,
		GID:       1000,
	}
	ctx := WithContext(context.Background(), lc)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		InfoCtx(ctx, "test message", "count", i)
	}
}
