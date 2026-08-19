package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// checkServer is a recording stub for the manifest-check endpoint. It also
// answers the share listing so the no-argument (whole-store) path can be
// exercised.
type checkServer struct {
	*httptest.Server
	paths   []string
	shares  []apiclient.Share
	results map[string]*engine.ManifestCheckResult
}

func newCheckServer(t *testing.T) *checkServer {
	t.Helper()
	s := &checkServer{results: map[string]*engine.ManifestCheckResult{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/shares" {
			_ = json.NewEncoder(w).Encode(s.shares)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/shares/"), "/audit/manifest")
		res, ok := s.results[name]
		if r.Method != http.MethodPost || !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(apiclient.BlockStoreManifestCheckResult{Result: res})
	}))
	t.Cleanup(s.Close)
	return s
}

func withCheckTestServer(t *testing.T, url string) {
	t.Helper()
	origServer, origToken, origOutput := cmdutil.Flags.ServerURL, cmdutil.Flags.Token, cmdutil.Flags.Output
	origHoles := checkIncludeHoles
	cmdutil.Flags.ServerURL = url
	cmdutil.Flags.Token = "test-token"
	cmdutil.Flags.Output = "table"
	t.Cleanup(func() {
		cmdutil.Flags.ServerURL = origServer
		cmdutil.Flags.Token = origToken
		cmdutil.Flags.Output = origOutput
		checkIncludeHoles = origHoles
	})
}

func captureStdoutCheck(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// damagedResult is a share holding one payload with a claimed-but-uncovered
// leading page — the shape of the field report this command exists to answer.
func damagedResult() *engine.ManifestCheckResult {
	return &engine.ManifestCheckResult{
		Share:                  "myshare",
		FilesScanned:           42,
		SyncedHashesChecked:    true,
		PayloadsWithFindings:   1,
		DamagedPayloads:        1,
		UncoveredRanges:        1,
		UncoveredBytes:         4096,
		ClaimedUncoveredRanges: 1,
		ClaimedUncoveredBytes:  4096,
		Findings: []engine.PayloadFinding{{
			Path:      "/docs/report.pdf",
			PayloadID: "payload-1",
			Size:      1048576,
			Uncovered: []engine.ByteRange{{Start: 0, End: 4096, Claimed: true}},
		}},
	}
}

// TestCheckCmd_DamageExitsNonZero asserts the command hits the per-share
// endpoint, renders the affected path, and returns an error so the scan can
// gate a script.
func TestCheckCmd_DamageExitsNonZero(t *testing.T) {
	s := newCheckServer(t)
	s.results["myshare"] = damagedResult()
	withCheckTestServer(t, s.URL)

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if want := "POST /api/v1/shares/myshare/audit/manifest"; len(s.paths) != 1 || s.paths[0] != want {
		t.Fatalf("requests = %v, want [%s]", s.paths, want)
	}
	if runErr == nil {
		t.Fatal("damage must return a non-nil error (non-zero exit)")
	}
	for _, frag := range []string{"Files scanned", "42", "Damaged payloads", "/docs/report.pdf", "[0,4096) uncovered, claimed"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestCheckCmd_DamageExitsNonZeroAcrossFormats pins that -o json/yaml still
// exits non-zero — a machine consumer must not read exit 0 on corruption.
func TestCheckCmd_DamageExitsNonZeroAcrossFormats(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			s := newCheckServer(t)
			s.results["myshare"] = damagedResult()
			withCheckTestServer(t, s.URL)
			cmdutil.Flags.Output = format

			var runErr error
			out := captureStdoutCheck(t, func() {
				runErr = runStoreCheck(checkCmd, []string{"myshare"})
			})
			if runErr == nil {
				t.Fatalf("%s: damage must return a non-nil error", format)
			}
			if !strings.Contains(out, "report.pdf") {
				t.Errorf("%s: result body must still be emitted; got %q", format, out)
			}
		})
	}
}

// TestCheckCmd_UnclaimedHoleIsNotDamage is the false-positive guard: a sparse
// file's hole must neither fail the command nor clutter the default detail
// table, but must still be counted and reachable with --include-holes.
func TestCheckCmd_UnclaimedHoleIsNotDamage(t *testing.T) {
	sparse := func() *engine.ManifestCheckResult {
		return &engine.ManifestCheckResult{
			Share:                "myshare",
			FilesScanned:         1,
			PayloadsWithFindings: 1,
			UncoveredRanges:      1,
			UncoveredBytes:       4096,
			Findings: []engine.PayloadFinding{{
				Path:      "/sparse.img",
				PayloadID: "payload-1",
				Size:      8192,
				Uncovered: []engine.ByteRange{{Start: 0, End: 4096}},
			}},
		}
	}

	s := newCheckServer(t)
	s.results["myshare"] = sparse()
	withCheckTestServer(t, s.URL)

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})
	if runErr != nil {
		t.Fatalf("an unclaimed hole must not fail the command: %v", runErr)
	}
	if strings.Contains(out, "/sparse.img") {
		t.Errorf("unclaimed hole must stay out of the default detail; got %q", out)
	}
	if !strings.Contains(out, "--include-holes") {
		t.Errorf("output must point at --include-holes when holes were suppressed; got %q", out)
	}
	if !strings.Contains(out, "not checked (no remote store resolved") {
		t.Errorf("a skipped synced-hash check must say so rather than print 0; got %q", out)
	}

	checkIncludeHoles = true
	out = captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})
	if runErr != nil {
		t.Fatalf("--include-holes must not turn a hole into a failure: %v", runErr)
	}
	if !strings.Contains(out, "/sparse.img") || !strings.Contains(out, "hole, unclaimed") {
		t.Errorf("--include-holes must list the hole; got %q", out)
	}
}

// TestCheckCmd_NoArgScansEveryShare covers the whole-store path: list the
// shares, scan each, and fail if any one of them is damaged.
func TestCheckCmd_NoArgScansEveryShare(t *testing.T) {
	s := newCheckServer(t)
	s.shares = []apiclient.Share{{Name: "clean"}, {Name: "myshare"}}
	s.results["clean"] = &engine.ManifestCheckResult{Share: "clean", FilesScanned: 7}
	s.results["myshare"] = damagedResult()
	withCheckTestServer(t, s.URL)

	var runErr error
	captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, nil)
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "1 of 2 share(s)") {
		t.Fatalf("want a failure naming 1 of 2 shares, got %v", runErr)
	}
	want := []string{
		"GET /api/v1/shares",
		"POST /api/v1/shares/clean/audit/manifest",
		"POST /api/v1/shares/myshare/audit/manifest",
	}
	if len(s.paths) != len(want) {
		t.Fatalf("requests = %v, want %v", s.paths, want)
	}
	for i := range want {
		if s.paths[i] != want[i] {
			t.Fatalf("requests = %v, want %v", s.paths, want)
		}
	}
}

// TestCheckCmd_Registered guards against a refactor dropping the AddCommand
// call, which would silently remove the command from `dfsctl store --help`.
func TestCheckCmd_Registered(t *testing.T) {
	for _, sub := range Cmd.Commands() {
		if sub == checkCmd {
			return
		}
	}
	t.Fatal("checkCmd not registered on store.Cmd")
}
