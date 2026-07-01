//go:build e2e

package framework

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// NLMCaseResult is the outcome of one cross-protocol lock-interop case: a holder
// takes an exclusive byte-range lock via one protocol, a tester attempts a
// conflicting non-blocking lock via another, then the holder releases and the
// tester retries.
type NLMCaseResult struct {
	Conflict     string // BLOCKED (server denied the conflicting lock) or ACQUIRED
	AfterRelease string // ACQUIRED once the holder released, or NA
}

// nlmInteropBinaries are the host tools the netns-isolated real-NLM harness
// needs. Real NFSv3 NLM locking (no `nolock`) requires the server and client to
// live in separate network namespaces, each with its own rpcbind — see
// docs/internals/testing.md and issue #1503.
var nlmInteropBinaries = []string{
	"ip", "unshare", "rpcbind", "rpc.statd", "rpcinfo",
	"mount.nfs", "mount.cifs", "python3", "bash",
}

// sbinDirs are searched in addition to PATH for the harness's system tools,
// which commonly live in /usr/sbin or /sbin — directories a restricted (e.g.
// Nix) PATH may omit even though the tools are installed.
var sbinDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// lookTool resolves a binary via PATH, falling back to the well-known sbin
// directories so a trimmed PATH doesn't make the harness spuriously skip.
func lookTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range sbinDirs {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// SkipIfNLMInteropUnsupported skips unless the host can run the netns-isolated
// real-NLM interop harness: Linux, root (for namespaces + mounts), and all the
// required tooling available. This keeps the suite green (skipped) on macOS,
// unprivileged runners, and minimal images while still exercising the harness
// wherever it can run.
//
// Set DITTOFS_E2E_REQUIRE_NLM=1 (CI does) to turn any missing prerequisite into
// a hard failure instead of a skip — so a misconfigured runner can't silently
// drop this coverage.
func SkipIfNLMInteropUnsupported(t *testing.T) {
	t.Helper()
	require := os.Getenv("DITTOFS_E2E_REQUIRE_NLM") == "1"
	bail := func(format string, args ...any) {
		if require {
			t.Fatalf("NLM interop required but "+format, args...)
		}
		t.Skipf(format, args...)
	}
	if runtime.GOOS != "linux" {
		bail("harness requires Linux network namespaces")
		return
	}
	if os.Geteuid() != 0 {
		bail("harness requires root (network namespaces, mounts)")
		return
	}
	for _, bin := range nlmInteropBinaries {
		if _, err := lookTool(bin); err != nil {
			bail("harness needs %q available: %v", bin, err)
			return
		}
	}
}

var nlmResultRE = regexp.MustCompile(`(\S+) conflict=(\S+) afterRelease=(\S+)`)

// RunNLMAxisInterop builds dfs/dfsctl, runs the netns-isolated driver script
// (test/e2e/testdata/nlm/nlm_axis_interop.sh), and returns the parsed
// per-direction results. The driver stands up a DittoFS server and a real
// kernel NFSv3 client in isolated network namespaces and exercises byte-range
// lock conflicts across the NLM, NFSv4, and SMB protocols.
func RunNLMAxisInterop(t *testing.T) map[string]NLMCaseResult {
	t.Helper()

	root := getProjectRoot(t)
	dfs := buildE2EBinary(t, root, "./cmd/dfs", "dfs")
	dfsctl := buildE2EBinary(t, root, "./cmd/dfsctl", "dfsctl")

	base := filepath.Join(root, "test", "e2e", "testdata", "nlm")
	script := filepath.Join(base, "nlm_axis_interop.sh")
	lockpy := filepath.Join(base, "nlmlock.py")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Resolve bash the same sbin-aware way as the skip guard, so a trimmed PATH
	// (which passed the guard via lookTool) doesn't fail exec here.
	bash, err := lookTool("bash")
	if err != nil {
		t.Fatalf("bash not found: %v", err)
	}
	cmd := exec.CommandContext(ctx, bash, script, lockpy)
	cmd.Env = append(os.Environ(), "DFS_BIN="+dfs, "DFSCTL_BIN="+dfsctl)
	out, err := cmd.CombinedOutput()
	t.Logf("NLM interop driver output:\n%s", out)
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("NLM interop driver timed out")
	}
	if err != nil {
		t.Fatalf("NLM interop driver failed: %v", err)
	}

	results := make(map[string]NLMCaseResult)
	for _, m := range nlmResultRE.FindAllStringSubmatch(string(out), -1) {
		results[m[1]] = NLMCaseResult{Conflict: m[2], AfterRelease: m[3]}
	}
	if len(results) == 0 {
		t.Fatalf("no NLM interop case results parsed from driver output")
	}
	return results
}

// buildE2EBinary compiles a command in the repo to a temp path and returns it.
func buildE2EBinary(t *testing.T, root, pkg, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build %s: %v\n%s", name, err, combined)
	}
	return out
}
