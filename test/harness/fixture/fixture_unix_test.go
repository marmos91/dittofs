//go:build !windows

package fixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
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
	s, err := Start(ctx, registry.Fixture{Profile: "memory"}, nil, bin, out, &bytes.Buffer{})
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
	_, err := Start(context.Background(), registry.Fixture{Profile: "memory"}, nil, bin, out, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "dfs exited before its API answered (exit status 2)") {
		t.Errorf("Start: %v", err)
	}
	if log, _ := os.ReadFile(filepath.Join(out, "dfs.log")); !strings.Contains(string(log), "panic: broke") {
		t.Errorf("dfs.log: %q", log)
	}
}

// SMB on a set port, as WPTS's 445, starts dfs as dt's user with the one
// capability that binds it, through setpriv, and through sudo -n unless dt
// runs as root. sudo passes the admin's password on in the environment it
// resets, never on the command line dt shows. The setup's end stops it.
func TestStartOnSetPort(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	scripts := map[string]string{
		// sudo resets the environment to PATH, keeping what --preserve-env names.
		"sudo": `echo "sudo $*" >> ` + calls + `
[ "$1" = -n ] || exit 1; shift
case $1 in --preserve-env=DITTOFS_ADMIN_INITIAL_PASSWORD) shift; exec env -i "PATH=$PATH" "DITTOFS_ADMIN_INITIAL_PASSWORD=$DITTOFS_ADMIN_INITIAL_PASSWORD" "$@" ;; esac
exec env -i "PATH=$PATH" "$@"`,
		"setpriv": `echo "setpriv $*" >> ` + calls + `
while [ "$1" != -- ]; do shift; done; shift; exec "$@"`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	realEuid := geteuid
	t.Cleanup(func() { geteuid = realEuid })
	priv := fmt.Sprintf("setpriv --reuid=%d --regid=%d --init-groups --inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- ", os.Getuid(), os.Getgid())
	for _, tc := range []struct {
		euid int
		want []string // the calls, each up to dfs's arguments
	}{
		{1000, []string{"sudo -n --preserve-env=DITTOFS_ADMIN_INITIAL_PASSWORD " + priv, priv}},
		{0, []string{priv}},
	} {
		geteuid = func() int { return tc.euid }
		_ = os.Remove(calls)
		bin, starts := fakeDFS(t)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { started(starts); cancel() }()
		var w bytes.Buffer
		_, err := Start(ctx, registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Port: 445}}, nil, bin, t.TempDir(), &w)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("euid %d: Start: %v", tc.euid, err)
		}
		runs := started(starts)
		if len(runs) != 1 || len(runs[0]) != 3 || runs[0][2] != "DITTOFS_ADMIN_INITIAL_PASSWORD," {
			t.Fatalf("euid %d: dfs started as %q", tc.euid, runs)
		}
		if !ended(runs[0][0]) {
			t.Errorf("euid %d: dfs %s still runs", tc.euid, runs[0][0])
		}
		data, _ := os.ReadFile(calls)
		got := strings.Split(strings.TrimSpace(string(data)), "\n")
		dfs := filepath.Join(bin, "dfs") + " start --foreground --config "
		if len(got) != len(tc.want) || !strings.HasPrefix(got[0], tc.want[0]+dfs) || !strings.HasPrefix(got[len(got)-1], priv+dfs) {
			t.Errorf("euid %d: calls\n%s\nwant each to start\n%s", tc.euid, data, strings.Join(tc.want, dfs+"...\n"))
		}
		if !strings.Contains(w.String(), "[dt] fixture: $ "+got[0]+"\n") || strings.Contains(w.String(), "DITTOFS_ADMIN_INITIAL_PASSWORD=") {
			t.Errorf("euid %d: dt's lines:\n%s", tc.euid, w.String())
		}
	}
}

// A real dfs, built from this checkout, starts on each profile, exports the
// share over NFS at {server} on its own port, gives the placeholders the
// fixture names, and stops; a "dfs start" dt didn't start survives the stop.
// badger-s3 runs where the S3 service it needs answers, as dt services
// starts it, and its {bucket} lists the keys under the fixture's prefix, the
// fixture's own probe among them, and none of another fixture's.
func TestRealServer(t *testing.T) {
	bin := realBin(t)
	other := outsider(t)
	no := false
	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	services := needs.ServiceEnv(root, []string{"service:localstack"})
	for _, profile := range []string{"memory", "badger", "badger-s3"} {
		t.Run(profile, func(t *testing.T) {
			if profile == "badger-s3" {
				if err := needs.Check(root, "service:localstack"); err != nil {
					t.Skipf("service:localstack: %v", err)
				}
			}
			out := t.TempDir()
			var w bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f := registry.Fixture{Profile: profile, NFS: registry.NFS{Version: "export"}, Squash: "root_to_admin", Users: []string{"65532"}, Delegations: &no, Lease: "30s"}
			s, err := Start(ctx, f, services, bin, out, &w)
			if err != nil {
				log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
				t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
			}
			vals := s.Placeholders()
			if got, want := slices.Sorted(maps.Keys(vals)), slices.Sorted(slices.Values(Placeholders(f))); !slices.Equal(got, want) {
				t.Errorf("placeholders %v, the fixture names %v", got, want)
			}
			if profile == "badger-s3" {
				checkBucket(t, vals["bucket"])
			}
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

// checkBucket checks that listing, a fixture's {bucket}, lists its probe and
// only keys under its prefix.
func checkBucket(t *testing.T, listing string) {
	t.Helper()
	_, prefix, _ := strings.Cut(listing, "prefix=")
	resp, err := http.Get(listing)
	if err != nil {
		t.Fatalf("{bucket}: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	keys := regexp.MustCompile(`<Key>([^<]*)</Key>`).FindAllStringSubmatch(string(body), -1)
	if resp.StatusCode != http.StatusOK || !slices.ContainsFunc(keys, func(k []string) bool { return k[1] == prefix+"dt-probe" }) ||
		slices.ContainsFunc(keys, func(k []string) bool { return !strings.HasPrefix(k[1], prefix) }) {
		t.Errorf("{bucket} %s answered %d:\n%s", listing, resp.StatusCode, body)
	}
}

// A real dfs serves the SMB suites' shares on a port of its own and, where
// root and setpriv are and nothing holds it, on 445, as dt's user. Where
// smbclient is, {user} with {password} lists the shares and makes a
// directory at the root of the first, {share}, and again once the reset has
// created the share again. Stop stops the server.
func TestRealSMB(t *testing.T) {
	bin := realBin(t)
	shares, _ := filepath.Abs(filepath.Join(smbConformance, "shares.yaml"))
	for _, port := range []int{0, 445} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			if port != 0 {
				if _, err := exec.LookPath("setpriv"); err != nil {
					t.Skip("no setpriv here")
				}
				for _, n := range []string{"root", "port:445"} {
					if err := needs.Check("", n); err != nil {
						t.Skipf("%s: %v", n, err)
					}
				}
			}
			out := t.TempDir()
			var w bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f := registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Encryption: "preferred", Port: port}, Shares: shares, Reset: "each"}
			s, err := Start(ctx, f, nil, bin, out, &w)
			if err != nil {
				log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
				t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
			}
			api := s.api
			defer func() {
				if err := s.Stop(); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if ready(context.Background(), api) == nil {
					t.Error("the API still answers")
				}
			}()
			vals := s.Placeholders()
			if strings.Contains(w.String(), vals["password"]) {
				t.Errorf("dt's lines show {password}:\n%s", w.String())
			}
			if info, err := os.Stat(filepath.Join(s.data, "controlplane.db")); err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
				t.Errorf("dfs doesn't run as dt's user: %v", err)
			}
			if vals["share"] != "smbbasic" {
				t.Errorf("{share} is %q, not the first share without its /", vals["share"])
			}
			smbclient, err := exec.LookPath("smbclient")
			if err != nil {
				t.Skip("no smbclient here")
			}
			run := func(args ...string) string {
				t.Helper()
				args = append([]string{"-p", vals["smb-port"], "-U", vals["user"] + "%" + vals["password"]}, args...)
				got, err := exec.Command(smbclient, args...).CombinedOutput()
				if err != nil {
					t.Fatalf("smbclient %s: %v\n%s", strings.Join(args[4:], " "), err, got)
				}
				return string(got)
			}
			list := run("-L", "//"+vals["server"])
			for _, name := range []string{"smbbasic", "smbencrypted", "fileshare", "hideunread", "change_notify_disabled", "smbnoncanon", "create_no_streams", "smbpersistent"} {
				if !strings.Contains(list, name) {
					t.Errorf("smbclient -L lacks %s:\n%s", name, list)
				}
			}
			share := "//" + vals["server"] + "/" + vals["share"]
			if got := run(share, "-c", "mkdir before; ls"); !strings.Contains(got, "before") {
				t.Fatalf("{user} made no directory at the root:\n%s", got)
			}
			if err := s.Reset(vals["share"]); err != nil {
				t.Fatal(err)
			}
			if got := run(share, "-c", "mkdir after; ls"); !strings.Contains(got, "after") {
				t.Errorf("{user} made no directory at the root after the reset:\n%s", got)
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
