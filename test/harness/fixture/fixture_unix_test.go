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

// fakeDFS writes a dfs into a new bin directory that never answers, as a
// server stuck in its start would. Each start appends a line to starts: its
// pid, the pid of the child it starts, and the names of the DITTOFS_
// variables it was given.
func fakeDFS(t *testing.T) (bin, starts string) {
	t.Helper()
	bin, starts = t.TempDir(), filepath.Join(t.TempDir(), "starts")
	script := "#!/bin/sh\nsleep 60 &\n" +
		"echo \"$$ $! $(env | grep '^DITTOFS_' | cut -d= -f1 | sort | tr '\\n' ,)\" >> " + starts + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "dfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, starts
}

// started waits up to a minute for the first start in starts and returns
// every start's fields, or nil when there was none.
func started(starts string) [][]string {
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		data, _ := os.ReadFile(starts)
		if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); lines[0] != "" {
			var out [][]string
			for _, l := range lines {
				out = append(out, strings.Fields(l))
			}
			return out
		}
	}
	return nil
}

// outsider starts a "dfs start" of the kind a developer runs, which dt didn't
// start.
func outsider(t *testing.T) *exec.Cmd {
	t.Helper()
	bin, _ := fakeDFS(t)
	cmd := exec.Command(filepath.Join(bin, "dfs"), "start", "--foreground")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	return cmd
}

// alive reports whether the process pid runs; one that ended and waits to be
// reaped doesn't.
func alive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// ended waits up to 10 seconds for the process pid to end.
func ended(pid string) bool {
	n, err := strconv.Atoi(pid)
	for end := time.Now().Add(10 * time.Second); err == nil && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if !alive(n) {
			return true
		}
	}
	return false
}

// A server whose setup ends before it answers is started once, with none of
// the caller's DITTOFS_ variables, and stopped with its whole process group,
// its data removed and its output kept; a "dfs start" dt didn't start
// survives.
func TestSetupEnds(t *testing.T) {
	other := outsider(t)
	bin, starts := fakeDFS(t)
	t.Setenv("DITTOFS_CONTROLPLANE_PORT", "1")
	out := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// End the setup once dfs runs, and late enough for a retry to show.
		started(starts)
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	s, err := Start(ctx, registry.Fixture{Profile: "memory"}, bin, out, &bytes.Buffer{})
	if s != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v, %v", s, err)
	}
	runs := started(starts)
	if len(runs) != 1 || len(runs[0]) != 3 {
		t.Fatalf("dfs started %d times: %q", len(runs), runs)
	}
	if run := runs[0]; !ended(run[0]) || !ended(run[1]) {
		t.Errorf("the fixture's dfs %s or its child %s still runs", run[0], run[1])
	}
	if env := runs[0][2]; env != "DITTOFS_ADMIN_INITIAL_PASSWORD," {
		t.Errorf("dfs was given the DITTOFS_ variables %s", env)
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
	bin := realBin(t)
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

// realBin builds dfs and dfsctl from this checkout into a new directory for a
// real server, and skips the test under -short, where they don't build, or
// where there is no address for {server}.
func realBin(t *testing.T) string {
	t.Helper()
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
	return bin
}
