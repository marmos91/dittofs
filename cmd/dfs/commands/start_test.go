package commands

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// TestStart_LegacyLayoutExitCode asserts the Phase 17 Plan 09 D-11
// boot-guard contract: when LoadSharesFromStore surfaces an error
// wrapping blockstore.ErrLegacyLayoutDetected, the start command must
// (1) print the multi-line operator directive to stderr,
// (2) exit with code 78 (EX_CONFIG).
//
// The test stubs the production `exitFn` indirection through a
// t.Cleanup-restored override so the captured exit code is observable
// in-process without spawning a subprocess (T-17-09-07: cleanup
// restores the original os.Exit binding before the next test runs).
//
// The test uses the package-internal helper `handleLoadSharesError`
// directly because the full `runStart` cobra path requires DB setup,
// admin-user creation, and config loading — orthogonal to the
// legacy-layout policy under test. The helper IS the production code
// path: `runStart` invokes it verbatim immediately after
// runtime.LoadSharesFromStore returns. Capturing the wrap shape
// (`share "<name>": share <path>: blockstore: legacy ...`) matches
// what runtime.LoadSharesFromStore produces when AddShare bubbles
// `ErrLegacyLayoutDetected` from `fs.NewFSStore`.
func TestStart_LegacyLayoutExitCode(t *testing.T) {
	// Stub exitFn to capture the exit code without terminating the process.
	origExit := exitFn
	t.Cleanup(func() { exitFn = origExit })
	exitCh := make(chan int, 1)
	exitFn = func(code int) {
		// Buffered channel size 1; non-blocking send avoids deadlock
		// on the unlikely case of multiple calls (defensive — the
		// production path calls exitFn at most once).
		select {
		case exitCh <- code:
		default:
		}
	}

	// Capture stderr via a pipe so we can assert on the directive text.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	// Synthesize the exact wrap shape runtime.LoadSharesFromStore
	// produces: `share %q: %w` around the fs.NewFSStore output, which
	// is itself `share %s: %w` around blockstore.ErrLegacyLayoutDetected.
	sharePath := filepath.Join(t.TempDir(), "share-A", "blocks")
	innerErr := fmt.Errorf("share %s: %w", sharePath, blockstore.ErrLegacyLayoutDetected)
	loadErr := fmt.Errorf("share %q: %w", "share-A", innerErr)

	stop := handleLoadSharesError(loadErr, w)
	if !stop {
		t.Fatalf("handleLoadSharesError returned stop=false on legacy-layout error")
	}

	// Close the writer so the reader sees EOF, then drain.
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var stderrBuf bytes.Buffer
	if _, err := stderrBuf.ReadFrom(r); err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	_ = r.Close()

	// Assert: exit code 78 was captured.
	var gotCode int
	select {
	case gotCode = <-exitCh:
	default:
		t.Fatalf("exitFn was never invoked on legacy-layout error")
	}
	if gotCode != EX_CONFIG {
		t.Errorf("captured exit code = %d, want %d", gotCode, EX_CONFIG)
	}
	if gotCode != 78 {
		t.Errorf("captured exit code = %d, want literal 78 (sysexits EX_CONFIG)", gotCode)
	}

	stderr := stderrBuf.String()
	for _, want := range []string{
		"Detected legacy .blk layout",
		"dfs migrate-to-cas",
		sharePath,
		"docs/CONFIGURATION.md",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q\nstderr was:\n%s", want, stderr)
		}
	}
}

// TestHandleLoadSharesError_NonLegacyContinues asserts the helper's
// warn-and-continue behavior for non-legacy errors (T-17-09-07
// non-regression — generic share-load failures must NOT trigger exit
// 78, preserving the historical best-effort behavior).
func TestHandleLoadSharesError_NonLegacyContinues(t *testing.T) {
	origExit := exitFn
	t.Cleanup(func() { exitFn = origExit })
	exitCh := make(chan int, 1)
	exitFn = func(code int) {
		select {
		case exitCh <- code:
		default:
		}
	}

	stop := handleLoadSharesError(errors.New("some other failure"), os.Stderr)
	if stop {
		t.Fatalf("handleLoadSharesError returned stop=true on non-legacy error")
	}
	select {
	case got := <-exitCh:
		t.Fatalf("exitFn called with %d on non-legacy error", got)
	default:
	}
}

// TestHandleLoadSharesError_NilNoop confirms the helper is a no-op on a
// nil error (no exit, no stop).
func TestHandleLoadSharesError_NilNoop(t *testing.T) {
	origExit := exitFn
	t.Cleanup(func() { exitFn = origExit })
	exitFn = func(code int) {
		t.Errorf("exitFn must not be called on nil error; got code=%d", code)
	}
	if handleLoadSharesError(nil, os.Stderr) {
		t.Errorf("handleLoadSharesError returned stop=true on nil error")
	}
}
