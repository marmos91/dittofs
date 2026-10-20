package fixture

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// When dt ends without stopping its fixture, as when it panics or is killed,
// dfs ends with it.
func TestDfsEndsWithDT(t *testing.T) {
	if bin := os.Getenv("DT_FIXTURE_TEST_BIN"); bin != "" {
		// The dt the test kills: its fixture never answers.
		_, _ = Start(context.Background(), registry.Fixture{Profile: "memory"}, nil, bin, os.Getenv("DT_FIXTURE_TEST_OUT"), io.Discard)
		return
	}
	bin, starts := fakeDFS(t)
	dt := exec.Command(os.Args[0], "-test.run=^TestDfsEndsWithDT$")
	dt.Env = append(os.Environ(), "DT_FIXTURE_TEST_BIN="+bin, "DT_FIXTURE_TEST_OUT="+t.TempDir())
	if err := dt.Start(); err != nil {
		t.Fatal(err)
	}
	runs := started(starts)
	_ = dt.Process.Kill()
	_ = dt.Wait()
	if len(runs) != 1 {
		t.Fatalf("dfs started %d times", len(runs))
	}
	// The fake's own child isn't the server's, so the test ends it.
	pid, _ := strconv.Atoi(runs[0][0])
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if !ended(runs[0][0]) {
		t.Errorf("dfs %s outlived the dt that started it", runs[0][0])
	}
}

// Where root and the NFS client are there, a real dfs's share is mounted at
// {mount} at each NFS version, with pjdfstest's options, and answers there;
// Stop unmounts it and removes {mount}.
func TestRealMount(t *testing.T) {
	for _, n := range []string{"root", "nfs-client"} {
		if err := needs.Check("", n); err != nil {
			t.Skipf("%s: %v", n, err)
		}
	}
	bin := realBin(t)
	for _, v := range []string{"3", "4", "4.1"} {
		t.Run(v, func(t *testing.T) {
			out := t.TempDir()
			var w bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f := registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: v, Options: "noac,sync,lookupcache=none"}, Squash: "none", Users: []string{"65534"}}
			s, err := Start(ctx, f, nil, bin, out, &w)
			if err != nil {
				log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
				t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
			}
			mount := s.Placeholders()["mount"]
			if _, err := os.ReadDir(mount); err != nil {
				t.Errorf("reading {mount}: %v", err)
			}
			// A suite's users reach the share through {mount}: the
			// directory under the mount is private to the user dt runs as.
			nobody := exec.Command("setpriv", "--reuid=65534", "--regid=65534", "--clear-groups", "ls", mount)
			if out, err := nobody.CombinedOutput(); err != nil {
				t.Errorf("a user other than root lists {mount}: %v\n%s", err, out)
			}
			if err := s.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
			}
			if _, mounted, _ := mountedAt(mount); mounted {
				t.Errorf("%s is still mounted", mount)
			}
			if _, err := os.Stat(mount); !os.IsNotExist(err) {
				t.Errorf("{mount} stays: %v\n%s", err, w.String())
			}
		})
	}
}

// Where root, the NFS client, the host's rpcbind and the client tools are
// there, a real dfs serves two namespaced clients. The first mounts the share
// at NFSv3 without nolock, so its locks go to the server's lock manager, which
// the host's rpcbind gives it, and its own rpcbind lists its own rpc.statd;
// each client has its own hostname, and the user dt runs as reaches the
// second over ssh. Stop leaves no namespace or bridge behind.
func TestRealClients(t *testing.T) {
	for _, n := range []string{"root", "nfs-client", "rpcbind"} {
		if err := needs.Check("", n); err != nil {
			t.Skipf("%s: %v", n, err)
		}
	}
	for _, tool := range []string{"/usr/sbin/sshd", "ssh", "nsenter", "unshare", "tc", "rpc.statd"} {
		if _, err := exec.LookPath(tool); err != nil {
			if _, err := exec.LookPath("/usr/sbin/" + tool); err != nil {
				t.Skipf("%s: %v", tool, err)
			}
		}
	}
	bin := realBin(t)
	f := registry.Fixture{Profile: "memory", NFS: registry.NFS{Options: "vers=3,tcp,mountproto=tcp", FromOptions: true}, Squash: "none", Client: "namespace", Clients: 2, RPCBind: "host"}
	if geteuid() != 0 {
		f.Users = []string{"self"}
	}
	out := t.TempDir()
	var w bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Start(ctx, f, nil, bin, out, &w)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
		t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
	}
	bridge := s.clients.bridge
	run := func(argv ...string) string {
		cmd, err := s.Command(argv, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, got)
		}
		return strings.TrimSpace(string(got))
	}
	mount := s.Placeholders()["mount"]
	if opts, _, err := s.mountedAt(mount); err != nil || !strings.Contains(opts, "vers=3") || !strings.Contains(opts, "local_lock=none") {
		t.Errorf("mounted with %q (%v), not at NFSv3 with its locks on the server", opts, err)
	}
	// decision: no lock is taken, since the server refuses new locks for
	// its grace period after it starts, and the first would wait it out.
	port, _ := strconv.Atoi(s.Placeholders()["nfs-port"])
	if got := run("rpcinfo", "-p", s.Placeholders()["server"]); len(unregistered(got, port)) > 0 {
		t.Errorf("from the client, the host's rpcbind lacks the server's %q:\n%s", unregistered(got, port), got)
	}
	if got := run("rpcinfo", "-p"); !strings.Contains(got, " status") {
		t.Errorf("the client's own rpcbind lists no rpc.statd:\n%s", got)
	}
	if got := run("hostname"); got != bridge+"-1" {
		t.Errorf("the first client's hostname is %q", got)
	}
	if got := run("ssh", s.Placeholders()["client2"], "hostname"); got != bridge+"-2" {
		t.Errorf("over ssh the second client says %q", got)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if list, _ := exec.Command(asRoot("ip", "netns", "list")[0], asRoot("ip", "netns", "list")[1:]...).Output(); strings.Contains(string(list), bridge+"-") {
		t.Errorf("namespaces stay:\n%s", list)
	}
	if exec.Command("ip", "link", "show", bridge).Run() == nil {
		t.Errorf("the bridge %s stays", bridge)
	}
}
