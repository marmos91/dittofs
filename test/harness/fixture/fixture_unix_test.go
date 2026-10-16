//go:build !windows

package fixture

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeDFS writes a dfs into a new bin directory that records the pid of each
// start in starts and never answers, as a server stuck in its start would.
func fakeDFS(t *testing.T) (bin, starts string) {
	t.Helper()
	bin, starts = t.TempDir(), filepath.Join(t.TempDir(), "starts")
	script := "#!/bin/sh\necho $$ >> " + starts + "\nsleep 60\n"
	if err := os.WriteFile(filepath.Join(bin, "dfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, starts
}

// outsider starts a "dfs start" of the kind a developer runs, which dt didn't
// start.
func outsider(t *testing.T) *exec.Cmd {
	t.Helper()
	bin, _ := fakeDFS(t)
	cmd := exec.Command(filepath.Join(bin, "dfs"), "start", "--foreground")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// alive reports whether a process with this pid still runs.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// A server that doesn't answer within the setup's limit is started once, and
// stopped with its process group, its data removed and its output kept; a
// "dfs start" dt didn't start survives.
func TestSetupLimit(t *testing.T) {
	other := outsider(t)
	bin, starts := fakeDFS(t)
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := Start(ctx, registry.Fixture{Profile: "memory"}, bin, out, &bytes.Buffer{})
	if s != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start: %v, %v", s, err)
	}
	pids, _ := os.ReadFile(starts)
	lines := strings.Fields(string(pids))
	if len(lines) != 1 {
		t.Fatalf("dfs started %d times: %q", len(lines), lines)
	}
	if pid, err := strconv.Atoi(lines[0]); err != nil || alive(pid) {
		t.Errorf("the fixture's dfs %s still runs", lines[0])
	}
	if !alive(other.Process.Pid) {
		t.Error("the dfs dt didn't start was stopped too")
	}
	if _, err := os.Stat(filepath.Join(out, "dfs")); !os.IsNotExist(err) {
		t.Errorf("the data stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "dfs.log")); err != nil {
		t.Errorf("the output is gone: %v", err)
	}
}

// A dfs that exits before it answers fails the setup at once, with how it
// exited, and its output stays for the cell.
func TestSetupServerExits(t *testing.T) {
	bin, _ := fakeDFS(t)
	if err := os.WriteFile(filepath.Join(bin, "dfs"), []byte("#!/bin/sh\necho 'panic: broke' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, err := Start(context.Background(), registry.Fixture{Profile: "memory"}, bin, out, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "dfs exited before its API answered (exit status 2)") {
		t.Errorf("Start: %v", err)
	}
	if log, _ := os.ReadFile(filepath.Join(out, "dfs.log")); !strings.Contains(string(log), "panic: broke") {
		t.Errorf("dfs.log: %q", log)
	}
}

// A real dfs, built from this checkout, starts on each profile, exports the
// share over NFS at {server} on its own port, and stops; a "dfs start" dt
// didn't start survives the stop.
func TestRealServer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs dfs")
	}
	if _, err := address(); err != nil {
		t.Skipf("no address for {server} here: %v", err)
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", bin+string(filepath.Separator), "./cmd/dfs", "./cmd/dfsctl")
	build.Dir = filepath.Join("..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("dfs doesn't build here, so no real server runs: %v\n%s", err, out)
	}
	other := outsider(t)
	no := false
	for _, profile := range []string{"memory", "badger"} {
		t.Run(profile, func(t *testing.T) {
			out := t.TempDir()
			var w bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f := registry.Fixture{Profile: profile, NFS: registry.NFS{Version: "export"}, Squash: "root_to_admin", Users: []int{65532}, Delegations: &no, Lease: "30s"}
			s, err := Start(ctx, f, bin, out, &w)
			if err != nil {
				log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
				t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
			}
			vals := s.Placeholders()
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(vals["server"], vals["nfs-port"]), 5*time.Second)
			if err != nil {
				t.Errorf("NFS at {server}:{nfs-port}: %v", err)
			} else {
				_ = conn.Close()
			}
			if strings.Contains(w.String(), vals["password"]) {
				t.Errorf("dt's lines show {password}:\n%s", w.String())
			}
			api := s.api
			if err := s.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
			}
			if ready(context.Background(), api) == nil {
				t.Error("the API still answers")
			}
			if _, err := os.Stat(filepath.Join(out, "dfs")); !os.IsNotExist(err) {
				t.Errorf("the data stays: %v", err)
			}
			if !alive(other.Process.Pid) {
				t.Error("the dfs dt didn't start was stopped too")
			}
		})
	}
}
