//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

// TestSMB3_ConcurrentSessionSetup_Issue362 mimics smbtorture bench.*'s
// "Open 4 connections" pattern that produces "Bad SMB2 (sign_algo_id=2)
// signature" failures in CI. The test starts N parallel goroutines that each
// dial a fresh TCP connection, perform NEGOTIATE+SESSION_SETUP, then issue
// one operation, then logoff. Repeated `iterations` times.
//
// If signing keys can be derived using cross-contaminated state (e.g.,
// shared preauth hash, shared session-setup stash), one or more of the
// concurrent SESSION_SETUPs will produce a response signed with the wrong
// key and the client will reject it.
func TestSMB3_ConcurrentSessionSetup_Issue362(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent SESSION_SETUP stress in short mode")
	}

	const (
		parallelDials = 8
		iterations    = 50
	)

	env := helpers.SetupSMB3TestEnv(t)

	var failures int64
	var failuresMu sync.Mutex
	var firstError error

	for iter := 0; iter < iterations; iter++ {
		var wg sync.WaitGroup
		startGate := make(chan struct{})
		for d := 0; d < parallelDials; d++ {
			wg.Add(1)
			go func(dialID int) {
				defer wg.Done()
				<-startGate // synchronize all goroutines to dial at the same instant
				sess, err := helpers.ConnectSMB3WithError(t, env.SMBPort, env.Username, env.Password)
				if err != nil {
					failuresMu.Lock()
					failures++
					if firstError == nil {
						firstError = fmt.Errorf("iter %d dial %d: %w", iter, dialID, err)
					}
					failuresMu.Unlock()
					return
				}
				_ = sess.Logoff()
			}(d)
		}
		// Release all goroutines at once for maximum contention.
		close(startGate)
		wg.Wait()
		if iter > 0 && iter%10 == 0 {
			t.Logf("iter %d/%d, failures so far: %d", iter, iterations, failures)
		}
		// Tiny gap between iterations
		time.Sleep(10 * time.Millisecond)
	}

	if failures > 0 {
		logPath := env.ServerProcess.LogFile()
		logBytes, _ := os.ReadFile(logPath)
		t.Logf("first error: %v", firstError)
		// Surface any signing trace anomalies that fired
		for _, marker := range []string{
			"SELF-VERIFY FAILED",
			"MISS on non-SESSION_SETUP",
			"SignMessage SKIPPED",
			"signer is NIL",
			"plaintext MUTATED",
		} {
			if strings.Contains(string(logBytes), marker) {
				t.Logf("SERVER TRACE: detected %q", marker)
			}
		}
		t.Errorf("CONCURRENT SESSION_SETUP RACE: %d/%d dials failed across %d iterations (%d concurrent each)",
			failures, int64(iterations*parallelDials), iterations, parallelDials)
	}
}

// TestSMB3_SigningStress_ScanFindRepro mimics the smbtorture scan.find pattern
// (issue #362) to fish for the AES-128-GMAC "Bad SMB2 signature" flake.
//
// scan.find opens a directory once, then issues many QUERY_DIRECTORY operations
// in rapid sequence with different FileInfoClass values. Each response must be
// signed correctly. Intermittent server-side bad-signature responses cause the
// client to reject the message.
//
// This test approximates the workload by issuing N back-to-back ReadDir calls
// per session across M parallel sessions. The server-side SIGN_TRACE
// instrumentation will fail loudly via SELF-VERIFY FAILED if any signing
// primitive misbehaves, and via "MISS on non-SESSION_SETUP" if a session
// lookup races with cleanup.
//
// After the test completes, we scan the server log file for any SIGN_TRACE
// anomalies and fail the test if found — turning the trace into a hard
// assertion rather than a passive log.
func TestSMB3_SigningStress_ScanFindRepro(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping signing stress test in short mode")
	}

	const (
		parallelSessions   = 32
		queriesPerSession  = 500
		entriesPerWorkdir  = 64
	)

	env := helpers.SetupSMB3TestEnv(t)

	// Pre-populate a shared workdir so ReadDir has stable output.
	setupSession := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	setupShare := helpers.MountSMB3Share(t, setupSession, env.ShareName)
	require.NoError(t, setupShare.Mkdir("stressdir", 0755))
	for i := 0; i < entriesPerWorkdir; i++ {
		name := fmt.Sprintf("stressdir/entry_%03d.txt", i)
		require.NoError(t, setupShare.WriteFile(name, []byte("x"), 0644))
	}

	// Hammer the server with parallel sessions, each doing many sequential
	// ReadDir calls on the shared dir. This generates dense, signed
	// QUERY_DIRECTORY traffic across multiple concurrent sessions.
	var wg sync.WaitGroup
	errCh := make(chan error, parallelSessions)
	for s := 0; s < parallelSessions; s++ {
		wg.Add(1)
		go func(sid int) {
			defer wg.Done()
			sess := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
			share := helpers.MountSMB3Share(t, sess, env.ShareName)
			for i := 0; i < queriesPerSession; i++ {
				if _, err := share.ReadDir("stressdir"); err != nil {
					errCh <- fmt.Errorf("session %d query %d: %w", sid, i, err)
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("client-visible error: %v", err)
	}

	// Drain server log for SIGN_TRACE anomalies. The log file is the e2e
	// framework's per-test temp file; helpers expose its path.
	logPath := env.ServerProcess.LogFile()
	data, err := os.ReadFile(logPath)
	require.NoError(t, err, "should read server log %s", logPath)
	logStr := string(data)

	if strings.Contains(logStr, "SELF-VERIFY FAILED") {
		t.Fatalf("SIGN_TRACE detected SELF-VERIFY FAILED in server log — #362 reproduced.\nLog: %s", logPath)
	}
	if strings.Contains(logStr, "MISS on non-SESSION_SETUP") {
		t.Fatalf("SIGN_TRACE detected session lookup MISS on non-SESSION_SETUP — silent unsigned response.\nLog: %s", logPath)
	}
	if strings.Contains(logStr, "SIGN_TRACE: Session.SignMessage SKIPPED") {
		// May be benign (LoggedOff sessions) — log but don't fail.
		t.Logf("SIGN_TRACE: session signing skipped (may be expected for LoggedOff sessions)")
	}

	// Sanity: confirm we actually exercised the signing path.
	signCount := strings.Count(logStr, "SIGN_TRACE: signed")
	t.Logf("SIGN_TRACE: %d signed responses across %d sessions x %d queries",
		signCount, parallelSessions, queriesPerSession)
	require.Greater(t, signCount, parallelSessions*queriesPerSession/2,
		"expected dense signed traffic — got %d, suggests signing not engaged", signCount)
}
