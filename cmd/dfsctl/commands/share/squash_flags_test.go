package share

import (
	"net/http"
	"strings"
	"testing"
)

// Refs #1566 — dfsctl flag plumbing for --squash on `share create`.
// Squash targets the per-share NFS adapter config endpoint, so a create with
// --squash must fire a PATCH to /adapters/nfs/config after the share POST.

func TestCreateCmd_Squash_PatchesNFSConfig(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetCreateFlags()
	createName = "/x"
	createMetadata = "meta"
	createLocal = "bs"
	createSquash = "none"
	if err := createCmd.Flags().Set("squash", "none"); err != nil {
		t.Fatalf("Flags.Set: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := runCreate(createCmd, nil); err != nil {
			t.Fatalf("runCreate: %v", err)
		}
	})

	// The last request is the squash PATCH, following the share POST.
	if s.lastVerb != http.MethodPatch {
		t.Fatalf("last verb = %q, want PATCH", s.lastVerb)
	}
	if !strings.HasSuffix(s.lastPath, "/adapters/nfs/config") {
		t.Fatalf("last path = %q, want .../adapters/nfs/config", s.lastPath)
	}
	if !strings.Contains(string(s.lastBody), `"squash":"none"`) {
		t.Errorf("squash PATCH body missing squash:none; got %s", s.lastBody)
	}
}

func TestCreateCmd_Squash_InvalidRejectedBeforeAnyRequest(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetCreateFlags()
	createName = "/x"
	createMetadata = "meta"
	createLocal = "bs"
	createSquash = "bogus"
	if err := createCmd.Flags().Set("squash", "bogus"); err != nil {
		t.Fatalf("Flags.Set: %v", err)
	}

	var runErr error
	_ = captureStdout(t, func() { runErr = runCreate(createCmd, nil) })

	if runErr == nil || !strings.Contains(runErr.Error(), "--squash") {
		t.Fatalf("want --squash validation error, got %v", runErr)
	}
	if s.lastVerb != "" {
		t.Errorf("no request should fire on invalid squash; got %s %s", s.lastVerb, s.lastPath)
	}
}
