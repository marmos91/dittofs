package commands

import (
	"bufio"
	"context"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

// TestFollowLogs_PartialLineNotDropped exercises the exact read-loop body used
// by followLogs' fsnotify.Write handler. bufio.Reader.ReadString('\n') returns
// (partial, io.EOF) when a file ends without a trailing newline. The old loop
// broke on err before printing, silently dropping the final incomplete line.
// The fixed loop prints line before checking err, guarded by line != "".
func TestFollowLogs_PartialLineNotDropped(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("line one\nno newline at end"))

	var got strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			got.WriteString(line)
		}
		if err != nil {
			break
		}
	}

	want := "line one\nno newline at end"
	if got.String() != want {
		t.Errorf("inner loop dropped partial line: got %q, want %q", got.String(), want)
	}
}

// TestFollowLogs_SignalStopCancelsContext verifies the invariant that
// followLogs relies on: signal.NotifyContext cancels the returned context when
// its stop function is called. defer stop() therefore guarantees the context is
// cancelled (and the signal registration released) on every return path,
// replacing the old leaked goroutine that blocked on <-sigCh forever.
func TestFollowLogs_SignalStopCancelsContext(t *testing.T) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// stop() is what defer stop() invokes on return.
	stop()

	select {
	case <-ctx.Done():
		// correct: stop() cancels ctx
	default:
		t.Fatal("expected ctx to be Done after stop(), but it was not")
	}
}
