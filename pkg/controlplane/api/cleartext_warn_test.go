package api

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// TestIsNonLoopbackHost is a table test for the host classifier that gates the
// cleartext-credentials warning.
func TestIsNonLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false},          // empty defaults to loopback (127.0.0.1)
		{"127.0.0.1", false}, // explicit loopback
		{"127.0.0.5", false}, // anything in 127.0.0.0/8 is loopback
		{"::1", false},
		{"[::1]", false},
		{"localhost", false},
		{"0.0.0.0", true}, // wildcard reaches off-host
		{"::", true},
		{"[::]", true},
		{"10.0.0.5", true},        // private but off-host
		{"192.168.1.20", true},    // off-host
		{"api.example.com", true}, // hostname → assume off-host
	}
	for _, c := range cases {
		if got := isNonLoopbackHost(c.host); got != c.want {
			t.Errorf("isNonLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

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
