package api

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// captureWarn redirects the global logger to a buffer at WARN level for the
// duration of the test. The logger output is process-global, so the tests that
// use this run serially (no t.Parallel).
var logCaptureMu sync.Mutex

func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	logCaptureMu.Lock()
	buf := &bytes.Buffer{}
	logger.InitWithWriter(buf, "WARN", "text", false)
	t.Cleanup(func() {
		// Restore a quiet default so later tests are unaffected.
		logger.InitWithWriter(&bytes.Buffer{}, "ERROR", "text", false)
		logCaptureMu.Unlock()
	})
	return buf
}

// TestNewServer_CleartextWarnOnNonLoopbackNoTLS asserts that binding the API to
// a non-loopback host with TLS disabled emits the cleartext-credentials WARN,
// and that a loopback bind (or a TLS-enabled non-loopback bind) does not.
func TestNewServer_CleartextWarnOnNonLoopbackNoTLS(t *testing.T) {
	const warnNeedle = "CLEARTEXT"

	t.Run("non-loopback, no TLS -> warns", func(t *testing.T) {
		buf := captureWarn(t)
		cpStore, cfg := testSetup(t, 0)
		t.Cleanup(func() { _ = cpStore.Close() })
		cfg.Host = "0.0.0.0"
		if _, err := NewServer(cfg, nil, cpStore, 30*time.Minute); err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if !strings.Contains(buf.String(), warnNeedle) {
			t.Fatalf("expected cleartext WARN for non-loopback no-TLS bind; log was:\n%s", buf.String())
		}
	})

	t.Run("loopback, no TLS -> no warn", func(t *testing.T) {
		buf := captureWarn(t)
		cpStore, cfg := testSetup(t, 0)
		t.Cleanup(func() { _ = cpStore.Close() })
		cfg.Host = "127.0.0.1"
		if _, err := NewServer(cfg, nil, cpStore, 30*time.Minute); err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if strings.Contains(buf.String(), warnNeedle) {
			t.Fatalf("did NOT expect cleartext WARN for loopback bind; log was:\n%s", buf.String())
		}
	})

	t.Run("non-loopback, TLS enabled -> no warn", func(t *testing.T) {
		buf := captureWarn(t)
		cpStore, cfg := testSetup(t, 0)
		t.Cleanup(func() { _ = cpStore.Close() })
		cfg.Host = "0.0.0.0"
		serverCert := generateCert(t, "localhost", []string{"localhost"}, false, nil)
		certPath, keyPath := writeCertFiles(t, t.TempDir(), serverCert)
		cfg.TLS = TLSConfig{CertFile: certPath, KeyFile: keyPath}
		if _, err := NewServer(cfg, nil, cpStore, 30*time.Minute); err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if strings.Contains(buf.String(), warnNeedle) {
			t.Fatalf("did NOT expect cleartext WARN when TLS is enabled; log was:\n%s", buf.String())
		}
	})
}
