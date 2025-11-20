package logger

import (
	"bytes"
	stdlog "log"
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
	originalLogger := logger
	logger = stdlog.New(buf, "", 0)

	cleanup := func() {
		logger = originalLogger
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

	t.Run("FormatsMessagesWithArguments", func(t *testing.T) {
		buf, cleanup := captureOutput()
		defer cleanup()

		SetLevel("INFO")
		Info("user %s has ID %d", "alice", 42)

		output := buf.String()
		assert.Contains(t, output, "user alice has ID 42")
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
					Info("goroutine %d log %d", id, j)
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
		_, cleanup := captureOutput()
		defer cleanup()

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
					Debug("debug %d", id)
					Info("info %d", id)
					Warn("warn %d", id)
					Error("error %d", id)
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
// Edge Cases Tests
// ============================================================================

func TestEdgeCases(t *testing.T) {
	t.Run("LogWithNilFormatArgs", func(t *testing.T) {
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
		Info("test %s %d %%", "special", 42)

		output := buf.String()
		assert.Contains(t, output, "special")
		assert.Contains(t, output, "42")
	})
}
