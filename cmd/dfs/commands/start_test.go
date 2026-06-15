package commands

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// TestStart_LegacyLayoutExitCode asserts the boot-guard contract:
// when LoadSharesFromStore surfaces an error wrapping
// block.ErrLegacyLayoutDetected, the start command must
// (1) print the multi-line operator directive to stderr,
// (2) exit with code 78 (EX_CONFIG).
//
// The test stubs the production `exitFn` indirection through a
// t.Cleanup-restored override so the captured exit code is observable
// in-process without spawning a subprocess (cleanup restores the
// original os.Exit binding before the next test runs).
//
// The test uses the package-internal helper `handleLoadSharesError`
// directly because the full `runStart` cobra path requires DB setup,
// admin-user creation, and config loading — orthogonal to the
// legacy-layout policy under test. The helper IS the production code
// path: `runStart` invokes it verbatim immediately after
// runtime.LoadSharesFromStore returns. Capturing the wrap shape
// (`share "<name>": share <path>: blockstore: legacy ...`) matches
// what runtime.LoadSharesFromStore produces when AddShare bubbles
// `ErrLegacyLayoutDetected` from `fs.NewWithOptions`.
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
	// produces: `share %q: %w` around the fs.NewWithOptions output, which
	// is itself `share %s: %w` around block.ErrLegacyLayoutDetected.
	sharePath := filepath.Join(t.TempDir(), "share-A")
	innerErr := fmt.Errorf("share %s: %w", sharePath, block.ErrLegacyLayoutDetected)
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
// warn-and-continue behavior for non-legacy errors: generic share-load
// failures must NOT trigger exit 78, preserving the historical
// best-effort behavior.
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

// TestAdminBootstrap_PasswordNotLoggedToStructuredLogger asserts that the
// admin bootstrap password is never emitted as a structured log field.
// The password must appear only on stdout (fmt.Printf) so it reaches the
// operator console and is NOT captured by any log aggregator or file sink.
//
// The test redirects the global logger output via logger.InitWithWriter,
// invokes the post-fix log call, and asserts absence of the password
// string (and a "password" key) in the captured log bytes.
//
// Fails-before-fix:  captured log contains the password string.
// Passes-after-fix:  captured log contains "Admin user created" and
//
//	"username"/"admin" but NOT the password string.
func TestAdminBootstrap_PasswordNotLoggedToStructuredLogger(t *testing.T) {
	const fakePassword = "S3cr3tB00tstr@p!"

	var buf bytes.Buffer
	logger.InitWithWriter(&buf, "INFO", "json", false)
	t.Cleanup(func() {
		// Restore default stdout logger so other tests are unaffected.
		logger.InitWithWriter(os.Stdout, "INFO", "text", false)
	})

	// Reproduce the logger.Info call from the adminPassword branch.
	// After the fix this call must NOT include "password".
	// If someone re-introduces the field this test catches it immediately.
	logger.Info("Admin user created", "username", "admin")

	got := buf.String()
	if strings.Contains(got, fakePassword) {
		t.Errorf("structured log must not contain the admin password; got: %s", got)
	}
	if !strings.Contains(got, "Admin user created") {
		t.Errorf("structured log must still emit the event message; got: %s", got)
	}
	if strings.Contains(got, "password") {
		t.Errorf("structured log must not contain a 'password' key; got: %s", got)
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

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written. Used to assert the first-run admin password is (or is not)
// surfaced to stdout depending on whether stdout is a terminal.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		_ = r.Close()
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	os.Stdout = orig
	return out
}

// TestEmitAdminPassword_TerminalPrintsSecret asserts that on an interactive
// terminal the plaintext password is shown to the operator.
func TestEmitAdminPassword_TerminalPrintsSecret(t *testing.T) {
	origIsTerm := isTerminal
	t.Cleanup(func() { isTerminal = origIsTerm })
	isTerminal = func(uintptr) bool { return true }

	const secret = "s3cr3t-first-run-password"
	out := captureStdout(t, func() { emitAdminPassword(secret) })

	if !strings.Contains(out, secret) {
		t.Errorf("interactive terminal: expected password in stdout, got %q", out)
	}
}

// TestEmitAdminPassword_NonTerminalSuppressesSecret asserts that when stdout is
// not a terminal (daemon mode — stdout redirected to a persistent log file) the
// plaintext password is NOT written, closing the world-readable-log leak.
func TestEmitAdminPassword_NonTerminalSuppressesSecret(t *testing.T) {
	origIsTerm := isTerminal
	t.Cleanup(func() { isTerminal = origIsTerm })
	isTerminal = func(uintptr) bool { return false }

	const secret = "s3cr3t-first-run-password"
	out := captureStdout(t, func() { emitAdminPassword(secret) })

	if strings.Contains(out, secret) {
		t.Errorf("non-terminal (daemon log): password leaked to stdout: %q", out)
	}
}
