package runtime

import (
	"testing"
	"time"
)

// TestSettingsWatcher_StopWithoutStart verifies that Stop() returns immediately
// when Start() was never called. Before the fix this would deadlock forever
// because <-w.stopped would block (no goroutine ever closes it).
func TestSettingsWatcher_StopWithoutStart(t *testing.T) {
	watcher := NewSettingsWatcher(nil, DefaultPollInterval)

	done := make(chan struct{})
	go func() {
		watcher.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop() returned without blocking — correct
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() deadlocked when called before Start()")
	}
}
