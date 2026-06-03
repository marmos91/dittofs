package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/bench/orchestrator"
)

// fakeExec records every interaction and serves a canned result file on Pull,
// so the full Orchestrator.Run flow is exercised without touching a network.
type fakeExec struct {
	pushed   []string // "local->remote"
	ran      []string // command strings
	pulled   []string // "remote->local"
	resultJS string   // written to the local path on Pull
	runErr   error
	pushErr  error
	pullErr  error
}

func (f *fakeExec) Run(_ context.Context, host, user, cmd string) ([]byte, error) {
	f.ran = append(f.ran, cmd)
	return nil, f.runErr
}

func (f *fakeExec) Push(_ context.Context, local, host, user, remote string) error {
	f.pushed = append(f.pushed, local+"->"+remote)
	return f.pushErr
}

func (f *fakeExec) Pull(_ context.Context, host, user, remote, local string) error {
	f.pulled = append(f.pulled, remote+"->"+local)
	if f.pullErr != nil {
		return f.pullErr
	}
	return os.WriteFile(local, []byte(f.resultJS), 0o644)
}

func goodResultJSON(t *testing.T) string {
	t.Helper()
	doc := orchestrator.NewDocument("r1", "2026-01-02T15:04:05Z", "sha", orchestrator.System{OS: "linux", Arch: "amd64"})
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestOrchestratorRun_HappyPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "dfsbench")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	localOut := filepath.Join(dir, "result.json")

	fe := &fakeExec{resultJS: goodResultJSON(t)}
	plan := Plan{
		Target:           Target{PublicIP: "1.2.3.4", PrivateIP: "10.0.0.5", User: "root"},
		LocalBinary:      bin,
		RemoteBinary:     "/usr/local/bin/dfsbench",
		RemoteResultPath: "/tmp/r.json",
		LocalResultPath:  localOut,
	}
	doc, err := New(fe).Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if doc.SchemaVersion != orchestrator.SchemaVersion {
		t.Errorf("schema_version = %d", doc.SchemaVersion)
	}
	// Binary pushed, chmod + bench ran, result pulled — in order.
	if len(fe.pushed) != 1 || !strings.Contains(fe.pushed[0], "->/usr/local/bin/dfsbench") {
		t.Errorf("push = %v", fe.pushed)
	}
	if len(fe.ran) != 2 {
		t.Fatalf("expected chmod + bench, got %v", fe.ran)
	}
	if !strings.HasPrefix(fe.ran[0], "chmod +x") {
		t.Errorf("first command should chmod the binary: %q", fe.ran[0])
	}
	if !strings.Contains(fe.ran[1], "orchestrate --out") {
		t.Errorf("bench command malformed: %q", fe.ran[1])
	}
	if len(fe.pulled) != 1 || !strings.Contains(fe.pulled[0], "/tmp/r.json->"+localOut) {
		t.Errorf("pull = %v", fe.pulled)
	}
}

// trackedExec also records the SSH destination host for each Run so a test can
// assert SSH only ever targets the public IP.
type trackedExec struct {
	fakeExec
	hosts []string
}

func (t *trackedExec) Run(ctx context.Context, host, user, cmd string) ([]byte, error) {
	t.hosts = append(t.hosts, host)
	return t.fakeExec.Run(ctx, host, user, cmd)
}

func (t *trackedExec) Push(ctx context.Context, local, host, user, remote string) error {
	t.hosts = append(t.hosts, host)
	return t.fakeExec.Push(ctx, local, host, user, remote)
}

func (t *trackedExec) Pull(ctx context.Context, host, user, remote, local string) error {
	t.hosts = append(t.hosts, host)
	return t.fakeExec.Pull(ctx, host, user, remote, local)
}

func TestOrchestratorRun_PublicIPForSSH_PrivateForMount(t *testing.T) {
	dir := t.TempDir()
	fe := &trackedExec{fakeExec: fakeExec{resultJS: goodResultJSON(t)}}
	plan := Plan{
		Target:           Target{PublicIP: "203.0.113.9", PrivateIP: "10.9.9.9", User: "root"},
		RemoteBinary:     "/usr/local/bin/dfsbench",
		RemoteResultPath: "/tmp/r.json",
		LocalResultPath:  filepath.Join(dir, "out.json"),
	}
	if _, err := New(fe).Run(context.Background(), plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Every SSH/scp interaction targets the PUBLIC IP — never the private one.
	for _, h := range fe.hosts {
		if h != "203.0.113.9" {
			t.Errorf("SSH targeted %q, want the public IP 203.0.113.9", h)
		}
	}
	// The PRIVATE IP appears ONLY as the mount env on the bench command, never
	// as the SSH transport target.
	benchCmd := fe.ran[len(fe.ran)-1]
	if !strings.Contains(benchCmd, MountIPEnv+"='10.9.9.9'") {
		t.Errorf("bench command missing private mount IP env: %q", benchCmd)
	}
}

func TestOrchestratorRun_ManifestPushed(t *testing.T) {
	dir := t.TempDir()
	man := filepath.Join(dir, "m.json")
	if err := os.WriteFile(man, []byte(`{"workloads":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExec{resultJS: goodResultJSON(t)}
	plan := Plan{
		Target:             Target{PublicIP: "1.1.1.1", User: "root"},
		RemoteBinary:       "/usr/local/bin/dfsbench",
		RemoteResultPath:   "/tmp/r.json",
		LocalResultPath:    filepath.Join(dir, "out.json"),
		ManifestPath:       man,
		RemoteManifestPath: "/tmp/m.json",
	}
	if _, err := New(fe).Run(context.Background(), plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fe.pushed) != 1 || !strings.Contains(fe.pushed[0], "->/tmp/m.json") {
		t.Errorf("manifest not pushed: %v", fe.pushed)
	}
	if !strings.Contains(fe.ran[len(fe.ran)-1], "--manifest '/tmp/m.json'") {
		t.Errorf("bench command missing --manifest: %q", fe.ran)
	}
}

func TestOrchestratorRun_RunError(t *testing.T) {
	dir := t.TempDir()
	fe := &fakeExec{runErr: errors.New("ssh boom")}
	plan := Plan{
		Target:           Target{PublicIP: "1.1.1.1", User: "root"},
		RemoteBinary:     "/usr/local/bin/dfsbench",
		RemoteResultPath: "/tmp/r.json",
		LocalResultPath:  filepath.Join(dir, "out.json"),
	}
	if _, err := New(fe).Run(context.Background(), plan); err == nil {
		t.Fatal("expected run error")
	}
}

func TestOrchestratorRun_RejectsBadVersionResult(t *testing.T) {
	dir := t.TempDir()
	fe := &fakeExec{resultJS: `{"schema_version":999,"workloads":{}}`}
	plan := Plan{
		Target:           Target{PublicIP: "1.1.1.1", User: "root"},
		RemoteBinary:     "/usr/local/bin/dfsbench",
		RemoteResultPath: "/tmp/r.json",
		LocalResultPath:  filepath.Join(dir, "out.json"),
	}
	_, err := New(fe).Run(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("expected version-mismatch rejection, got %v", err)
	}
}

func TestOrchestratorRun_NilExecutor(t *testing.T) {
	if _, err := (&Orchestrator{}).Run(context.Background(), Plan{}); err == nil {
		t.Fatal("nil executor must error")
	}
}

func TestBenchCommand(t *testing.T) {
	p := Plan{
		RemoteBinary:       "/usr/local/bin/dfsbench",
		RemoteResultPath:   "/tmp/r.json",
		RemoteManifestPath: "/tmp/m.json",
		RunID:              "run 1",
		Timestamp:          "2026-01-02T15:04:05Z",
		GitSHA:             "abc",
	}
	cmd := p.BenchCommand()
	for _, want := range []string{
		"/usr/local/bin/dfsbench orchestrate",
		"--out '/tmp/r.json'",
		"--manifest '/tmp/m.json'",
		"--run-id 'run 1'", // spaces survive via quoting
		"--timestamp '2026-01-02T15:04:05Z'",
		"--git-sha 'abc'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"plain":       "'plain'",
		"with space":  "'with space'",
		"it's quoted": `'it'\''s quoted'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanValidate(t *testing.T) {
	base := Plan{
		Target:           Target{PublicIP: "1.1.1.1", User: "root"},
		RemoteBinary:     "/bin/x",
		RemoteResultPath: "/tmp/r.json",
		LocalResultPath:  "out.json",
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	bad := base
	bad.RemoteBinary = ""
	if err := bad.Validate(); err == nil {
		t.Error("missing remote binary accepted")
	}
	// Manifest set but no remote manifest path → error.
	dir := t.TempDir()
	man := filepath.Join(dir, "m.json")
	_ = os.WriteFile(man, []byte("{}"), 0o644)
	bad2 := base
	bad2.ManifestPath = man
	if err := bad2.Validate(); err == nil {
		t.Error("manifest without remote path accepted")
	}
	// Nonexistent local binary → error.
	bad3 := base
	bad3.LocalBinary = filepath.Join(dir, "nope")
	if err := bad3.Validate(); err == nil {
		t.Error("nonexistent local binary accepted")
	}
}

func TestTargetValidate(t *testing.T) {
	if err := (Target{User: "root"}).Validate(false); err == nil {
		t.Error("missing public IP accepted")
	}
	if err := (Target{PublicIP: "1.1.1.1"}).Validate(false); err == nil {
		t.Error("missing user accepted")
	}
	// requirePrivate enforces the private IP (B5).
	if err := (Target{PublicIP: "1.1.1.1", User: "root"}).Validate(true); err == nil {
		t.Error("missing private IP accepted under requirePrivate")
	}
	if err := (Target{PublicIP: "1.1.1.1", PrivateIP: "10.0.0.1", User: "root"}).Validate(true); err != nil {
		t.Errorf("complete target rejected: %v", err)
	}
}
