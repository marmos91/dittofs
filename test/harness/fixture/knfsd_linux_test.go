package fixture

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// Where root, the kernel's NFS server and client are there, and nothing
// serves on 2049, a real knfsd fixture serves /export at the fixture's lease,
// and root creates files there as root, over NFSv4.1 and NFSv4.0. Stop stops
// the server and rpc.mountd, removes the export and restores the lease.
//
// Like the other real tests, it runs wherever its needs hold, except under
// -short, and changes what the fixture changes on the host: it loads the
// kernel's NFS modules, leaves the nfsd file system mounted, and runs the
// host's NFS server, at the fixture's lease and grace times, while it runs.
func TestRealKnfsd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the kernel's NFS server")
	}
	for _, n := range []string{"root", "knfsd", "nfs-client", "port:2049"} {
		if err := needs.Check("", n); err != nil {
			t.Skipf("%s: %v", n, err)
		}
	}
	if _, err := address(); err != nil {
		t.Skipf("no address for {server} here: %v", err)
	}
	root := func(argv ...string) string {
		argv = asRoot(argv...)
		got, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, got)
		}
		return strings.TrimSpace(string(got))
	}
	if _, err := os.Stat(filepath.Join(nfsdDir, "threads")); err != nil {
		root("mount", "-t", "nfsd", "nfsd", nfsdDir)
	}
	lease := root("cat", filepath.Join(nfsdDir, "nfsv4leasetime"))
	out := t.TempDir()
	var w bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	k, err := StartKnfsd(ctx, registry.Fixture{Server: "knfsd", NFS: registry.NFS{Version: "export"}, Squash: "none", Lease: "30s"}, out, &w)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(out, "mountd.log"))
		t.Fatalf("StartKnfsd: %v\n%s\nmountd.log:\n%s", err, w.String(), log)
	}
	t.Cleanup(func() { _ = k.Stop() })
	if got := root("cat", filepath.Join(nfsdDir, "nfsv4leasetime")); got != "30" {
		t.Errorf("the server's lease is %ss", got)
	}
	for _, vers := range []string{"4.1", "4.0"} {
		mount, err := os.MkdirTemp("/tmp", "dt")
		if err != nil {
			t.Fatal(err)
		}
		root("mount", "-t", "nfs", "-o", "vers="+vers+",port="+k.Placeholders()["nfs-port"], k.Placeholders()["server"]+":"+Share, mount)
		file := filepath.Join(mount, "by-root-"+vers)
		root("touch", file)
		if fi, err := os.Stat(file); err != nil {
			t.Errorf("NFSv%s: %v", vers, err)
		} else if uid := fi.Sys().(*syscall.Stat_t).Uid; uid != 0 {
			t.Errorf("NFSv%s: a file root creates is uid %d's", vers, uid)
		}
		root("umount", mount)
		_ = os.Remove(mount)
	}
	if err := k.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if got := root("cat", filepath.Join(nfsdDir, "threads")); got != "0" {
		t.Errorf("after Stop, the server runs %s threads", got)
	}
	if got := root("cat", filepath.Join(nfsdDir, "nfsv4leasetime")); got != lease {
		t.Errorf("after Stop, the lease is %ss, not the %ss it was", got, lease)
	}
	if got := root("exportfs", "-v"); strings.Contains(got, "dt-knfsd-") {
		t.Errorf("the export stays:\n%s", got)
	}
	if exec.Command("pgrep", "-x", "rpc.mountd").Run() == nil {
		t.Error("rpc.mountd runs after Stop")
	}
}
