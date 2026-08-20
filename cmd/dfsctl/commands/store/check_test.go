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
	paths  []string
	shares []apiclient.Share
	// opts records the repair switches of every scan request in order, so a
	// test can assert what the command asked the server to do.
	opts    []apiclient.BlockStoreManifestCheckOptions
	results map[string]*engine.ManifestCheckResult
	// respond overrides results when set, so a test can answer the plan, the
	// apply and the re-scan differently within one run.
	respond func(name string, opts apiclient.BlockStoreManifestCheckOptions) *engine.ManifestCheckResult
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
		var opts apiclient.BlockStoreManifestCheckOptions
		_ = json.NewDecoder(r.Body).Decode(&opts)
		s.opts = append(s.opts, opts)
		var res *engine.ManifestCheckResult
		ok := false
		if s.respond != nil {
			res = s.respond(name, opts)
			ok = res != nil
		} else {
			res, ok = s.results[name]
		}
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
	origHoles, origRepair, origYes := checkIncludeHoles, checkRepair, checkRepairYes
	cmdutil.Flags.ServerURL = url
	cmdutil.Flags.Token = "test-token"
	cmdutil.Flags.Output = "table"
	t.Cleanup(func() {
		cmdutil.Flags.ServerURL = origServer
		cmdutil.Flags.Token = origToken
		cmdutil.Flags.Output = origOutput
		checkIncludeHoles, checkRepair, checkRepairYes = origHoles, origRepair, origYes
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
			SyncedCheckSkipped:   "share has no remote store",
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
	if !strings.Contains(out, "not checked (share has no remote store") {
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

// TestCheckCmd_SkipReasonDistinguishesCases pins that the two conditions
// suppressing the unknown-hash check are reported apart. A share with no
// remote store has nothing that check could find; a share whose block store
// could not be resolved has a check that could not be run, and telling the
// second operator the first thing sends them away from a real fault.
func TestCheckCmd_SkipReasonDistinguishesCases(t *testing.T) {
	for _, reason := range []string{
		"share has no remote store",
		"block store could not be resolved for this share",
	} {
		s := newCheckServer(t)
		s.results["myshare"] = &engine.ManifestCheckResult{
			Share:              "myshare",
			FilesScanned:       1,
			SyncedCheckSkipped: reason,
		}
		withCheckTestServer(t, s.URL)

		out := captureStdoutCheck(t, func() {
			if err := runStoreCheck(checkCmd, []string{"myshare"}); err != nil {
				t.Fatalf("runStoreCheck: %v", err)
			}
		})
		if !strings.Contains(out, "not checked ("+reason+")") {
			t.Errorf("output must name the skip reason %q; got %q", reason, out)
		}
	}
}

// repairPlanResult is the damaged share with one repairable finding: a claimed
// range whose hash the remote resolves.
func repairPlanResult() *engine.ManifestCheckResult {
	r := damagedResult()
	r.RepairsPlanned = 1
	r.Repairs = []engine.RepairAction{{
		Kind:      engine.RepairRecreateRow,
		Path:      "/docs/report.pdf",
		PayloadID: "payload-1",
		Offset:    0,
		Size:      4096,
		ToRowID:   "payload-1/0",
	}}
	return r
}

// repairStages answers a whole --repair run: the plan, then the apply, then the
// read-only re-scan that reports the store as it now stands.
func repairStages() func(string, apiclient.BlockStoreManifestCheckOptions) *engine.ManifestCheckResult {
	return func(_ string, opts apiclient.BlockStoreManifestCheckOptions) *engine.ManifestCheckResult {
		switch {
		case opts.ApplyRepairs:
			r := repairPlanResult()
			r.RepairsApplied = 1
			r.Repairs[0].Applied = true
			return r
		case opts.PlanRepairs:
			return repairPlanResult()
		default:
			return &engine.ManifestCheckResult{Share: "myshare", FilesScanned: 42, SyncedHashesChecked: true}
		}
	}
}

// withConfirmAnswer feeds the confirmation prompt and keeps its output out of
// the captured stdout the assertions read.
func withConfirmAnswer(t *testing.T, answer string) {
	t.Helper()
	origIn, origOut := cmdutil.ConfirmInput, cmdutil.ConfirmOutput
	cmdutil.ConfirmInput = strings.NewReader(answer + "\n")
	cmdutil.ConfirmOutput = io.Discard
	t.Cleanup(func() {
		cmdutil.ConfirmInput = origIn
		cmdutil.ConfirmOutput = origOut
	})
}

// TestCheckCmd_RepairIsDryRunUntilConfirmed pins the safety property of the
// flag: --repair on its own asks the server to plan and nothing else, and a
// refused prompt leaves the store untouched.
func TestCheckCmd_RepairIsDryRunUntilConfirmed(t *testing.T) {
	s := newCheckServer(t)
	s.respond = repairStages()
	withCheckTestServer(t, s.URL)
	withConfirmAnswer(t, "n")
	checkRepair = true

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if len(s.opts) != 1 {
		t.Fatalf("want a single scan, got %d: %+v", len(s.opts), s.opts)
	}
	if s.opts[0].ApplyRepairs || !s.opts[0].PlanRepairs {
		t.Fatalf("dry run asked the server to write: %+v", s.opts[0])
	}
	if runErr == nil {
		t.Fatal("unrepaired damage must still exit non-zero")
	}
	for _, frag := range []string{"write row for claim", "payload-1/0", "Aborted"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestCheckCmd_RepairAppliesOnConfirm asserts a confirmed run writes once and
// then re-reads the store, so the verdict it prints and the exit code it
// returns describe the store after the repair rather than before it.
func TestCheckCmd_RepairAppliesOnConfirm(t *testing.T) {
	s := newCheckServer(t)
	s.respond = repairStages()
	withCheckTestServer(t, s.URL)
	withConfirmAnswer(t, "y")
	checkRepair = true

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if len(s.opts) != 3 {
		t.Fatalf("want plan, apply and re-scan, got %+v", s.opts)
	}
	if s.opts[0].ApplyRepairs {
		t.Fatalf("the planning scan wrote: %+v", s.opts[0])
	}
	if !s.opts[1].ApplyRepairs {
		t.Fatalf("the confirmed scan did not write: %+v", s.opts[1])
	}
	if s.opts[2].PlanRepairs || s.opts[2].ApplyRepairs {
		t.Fatalf("the verifying re-scan was not read-only: %+v", s.opts[2])
	}
	if runErr != nil {
		t.Fatalf("a fully repaired store must exit zero, got %v", runErr)
	}
	for _, frag := range []string{"Applied 1 repair(s), skipped 0", "Store as it stands after the repair"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestCheckCmd_RepairYesSkipsThePrompt asserts --yes writes on the first scan
// without reading the prompt at all.
func TestCheckCmd_RepairYesSkipsThePrompt(t *testing.T) {
	s := newCheckServer(t)
	s.respond = repairStages()
	withCheckTestServer(t, s.URL)
	// Any read of the prompt would block on an empty reader.
	withConfirmAnswer(t, "")
	checkRepair, checkRepairYes = true, true

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if len(s.opts) != 2 || !s.opts[0].ApplyRepairs {
		t.Fatalf("want an immediate apply then a re-scan, got %+v", s.opts)
	}
	if runErr != nil {
		t.Fatalf("a fully repaired store must exit zero, got %v", runErr)
	}
	if !strings.Contains(out, "Applied 1 repair(s)") {
		t.Errorf("stdout missing the outcome, got %q", out)
	}
}

// TestCheckCmd_RepairRefusesToPromptIntoMachineOutput asserts that with a
// machine-readable format the run stops at the plan rather than printing a
// prompt into the middle of the document.
func TestCheckCmd_RepairRefusesToPromptIntoMachineOutput(t *testing.T) {
	s := newCheckServer(t)
	s.respond = repairStages()
	withCheckTestServer(t, s.URL)
	cmdutil.Flags.Output = "json"
	checkRepair = true

	var runErr error
	captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if len(s.opts) != 1 || s.opts[0].ApplyRepairs {
		t.Fatalf("machine output must not write, got %+v", s.opts)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "--yes") {
		t.Fatalf("error = %v, want one naming --yes", runErr)
	}
}

// TestCheckCmd_RepairKeepsMachineOutputClean asserts that with a
// machine-readable format nothing human-readable is printed after the
// document, including the note for a run with nothing to repair.
func TestCheckCmd_RepairKeepsMachineOutputClean(t *testing.T) {
	s := newCheckServer(t)
	s.results["myshare"] = damagedResult()
	withCheckTestServer(t, s.URL)
	cmdutil.Flags.Output = "json"
	checkRepair = true

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if runErr == nil {
		t.Fatal("unrepaired damage must still exit non-zero")
	}
	if strings.Contains(out, "Nothing to repair") {
		t.Errorf("human text landed in the JSON document: %q", out)
	}
	var decoded []*engine.ManifestCheckResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout is not a JSON document: %v (got %q)", err, out)
	}
}

// TestCheckCmd_RepairWithNothingToRepair asserts damage the command cannot
// prove repairable stops before the prompt and still exits non-zero.
func TestCheckCmd_RepairWithNothingToRepair(t *testing.T) {
	s := newCheckServer(t)
	s.results["myshare"] = damagedResult()
	withCheckTestServer(t, s.URL)
	checkRepair = true

	var runErr error
	out := captureStdoutCheck(t, func() {
		runErr = runStoreCheck(checkCmd, []string{"myshare"})
	})

	if len(s.opts) != 1 {
		t.Fatalf("want a single scan, got %+v", s.opts)
	}
	if runErr == nil {
		t.Fatal("unrepairable damage must still exit non-zero")
	}
	if !strings.Contains(out, "Nothing to repair") {
		t.Errorf("stdout missing the nothing-to-repair note, got %q", out)
	}
}
