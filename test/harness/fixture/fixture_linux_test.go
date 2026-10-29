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
			// The user dt runs as reads {mount} below, so it needs a grant
			// unless it is root.
			if geteuid() != 0 {
				f.Users = append(f.Users, "self")
			}
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
			argv := asRoot("setpriv", "--reuid=65534", "--regid=65534", "--clear-groups", "ls", mount)
			nobody := exec.Command(argv[0], argv[1:]...)
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

// Where root and the SMB client are there, a real dfs's default share is
// mounted over SMB at {mount}, as {user}, and root makes a file at its root,
// as a suite's commands do; Stop unmounts it and removes {mount}.
func TestRealSMBMount(t *testing.T) {
	for _, n := range []string{"root", "smb-client"} {
		if err := needs.Check("", n); err != nil {
			t.Skipf("%s: %v", n, err)
		}
	}
	bin := realBin(t)
	out := t.TempDir()
	var w bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Start(ctx, registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Mount: true}}, nil, bin, out, &w)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
		t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
	}
	mount := s.Placeholders()["mount"]
	argv := asRoot("touch", filepath.Join(mount, "made"))
	if got, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Errorf("root makes a file at {mount}: %v\n%s", err, got)
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
	table := run("rpcinfo", "-p", s.Placeholders()["server"])
	if missing, _ := unregistered(table, port); len(missing) > 0 {
		t.Errorf("from the client, the host's rpcbind lacks the server's %q:\n%s", missing, table)
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

// Where docker and smbclient are, a real dfs with Kerberos gets a KDC of the
// image test/smb-conformance/kdc builds: smbclient lists {share} at {server}
// as {user}, with a ticket from the KDC {krb5-conf} names for {realm}, and
// over NTLM with {user}'s principal, which the server's mapping resolves.
// Stop removes the KDC's container.
func TestRealKerberos(t *testing.T) {
	bin := realBin(t)
	out := t.TempDir()
	if repo := os.Getenv("DITTOFS_REPO"); os.Getenv("DT_IN_CONTAINER") == "1" && !strings.HasPrefix(out, repo+"/") {
		t.Skip("in dtc the Docker daemon sees the checkout and not /tmp, so the KDC's keytab wouldn't reach dfs: set TMPDIR to a directory in the checkout")
	}
	if err := needs.Check("", "docker"); err != nil {
		t.Skipf("docker: %v", err)
	}
	smbclient, err := exec.LookPath("smbclient")
	if err != nil {
		t.Skip("no smbclient here")
	}
	if got, err := exec.Command("docker", "build", "-q", "-t", KDCImage, filepath.Join(smbConformance, "kdc")).CombinedOutput(); err != nil {
		t.Fatalf("the KDC's image: %v\n%s", err, got)
	}
	shares, _ := filepath.Abs(filepath.Join(smbConformance, "shares.yaml"))
	var w bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Start(ctx, registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Shares: shares, Kerberos: true}, nil, bin, out, &w)
	if err != nil {
		kdc, _ := os.ReadFile(filepath.Join(out, "kdc.log"))
		t.Fatalf("Start: %v\n%s\nkdc.log:\n%s", err, w.String(), kdc)
	}
	name := s.kdc.name
	defer func() {
		if err := s.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if exec.Command("docker", "container", "inspect", name).Run() == nil {
			t.Errorf("the KDC's container %s stays", name)
		}
	}()
	vals := s.Placeholders()
	for _, kerberos := range []string{"required", "disabled"} {
		ls := exec.Command(smbclient, "//"+vals["server"]+"/"+vals["share"], "-p", vals["smb-port"],
			"-U", vals["user"]+"@"+vals["realm"]+"%"+vals["password"], "--use-kerberos="+kerberos, "-c", "ls")
		ls.Env = append(os.Environ(), "KRB5_CONFIG="+vals["krb5-conf"])
		if got, err := ls.CombinedOutput(); err != nil {
			t.Errorf("smbclient --use-kerberos=%s: %v\n%s", kerberos, err, got)
		}
	}
}
