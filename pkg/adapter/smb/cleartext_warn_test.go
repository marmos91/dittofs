package smb

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/logger"
)

// logCaptureMu serializes the process-global logger redirect used below.
var logCaptureMu sync.Mutex

func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	logCaptureMu.Lock()
	buf := &bytes.Buffer{}
	logger.InitWithWriter(buf, "WARN", "text", false)
	t.Cleanup(func() {
		logger.InitWithWriter(&bytes.Buffer{}, "ERROR", "text", false)
		logCaptureMu.Unlock()
	})
	return buf
}

// TestNew_CleartextWarnOnNonLoopbackDisabledEncryption asserts that creating an
// SMB adapter bound to a non-loopback address with encryption disabled emits the
// cleartext WARN, while a loopback bind or any non-disabled mode stays quiet.
func TestNew_CleartextWarnOnNonLoopbackDisabledEncryption(t *testing.T) {
	const wantFragment = "traverse the network in CLEARTEXT"

	cases := []struct {
		name     string
		bind     string
		mode     string
		wantWarn bool
	}{
		{"non_loopback_disabled", "0.0.0.0", "disabled", true},
		{"empty_bind_disabled", "", "disabled", true}, // empty = wildcard (":port")
		{"non_loopback_preferred", "0.0.0.0", "preferred", false},
		{"non_loopback_required", "0.0.0.0", "required", false},
		{"loopback_disabled", "127.0.0.1", "disabled", false},
		{"default_mode_non_loopback", "0.0.0.0", "", false}, // defaults to preferred
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := captureWarn(t)
			_ = New(Config{
				BindAddress: c.bind,
				Encryption:  EncryptionConfig{Mode: c.mode},
			})
			got := strings.Contains(buf.String(), wantFragment)
			if got != c.wantWarn {
				t.Errorf("warn emitted = %v, want %v (log: %q)", got, c.wantWarn, buf.String())
			}
		})
	}
}
