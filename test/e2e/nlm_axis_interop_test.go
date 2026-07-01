//go:build e2e

package e2e

import (
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestNLMAxisInterop proves DittoFS's unified lock manager coordinates
// byte-range locks across a REAL kernel NFSv3 client (NLM, no `nolock`) and the
// NFSv4 and SMB protocols. Because a single host cannot run both a userspace NLM
// server and a kernel NFSv3 client's lockd against one rpcbind, the driver
// isolates the client in its own network namespace — see
// docs/internals/testing.md ("Real NFSv3 NLM lock testing") and issue #1503.
//
// Each direction holds an exclusive lock via one protocol and asserts a
// conflicting lock via another is denied by the server (BLOCKED), then that the
// lock becomes acquirable once the holder releases (ACQUIRED).
func TestNLMAxisInterop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NLM interop tests in short mode")
	}
	framework.SkipIfNLMInteropUnsupported(t)

	results := framework.RunNLMAxisInterop(t)

	for _, name := range []string{"NLM_vs_SMB", "NLM_vs_NFSv4", "SMB_vs_NLM", "NFSv4_vs_NLM"} {
		t.Run(name, func(t *testing.T) {
			r, ok := results[name]
			if !ok {
				t.Fatalf("no result reported for %s", name)
			}
			if r.Conflict != "BLOCKED" {
				t.Errorf("conflicting lock should be denied by the server (BLOCKED), got %q", r.Conflict)
			}
			if r.AfterRelease != "ACQUIRED" {
				t.Errorf("lock should be acquirable after the holder releases (ACQUIRED), got %q", r.AfterRelease)
			}
		})
	}
}
