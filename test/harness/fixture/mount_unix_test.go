//go:build !windows

package fixture

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeMounts puts a sudo, a mount and a umount on PATH that log each call
// and keep a mountinfo table of their own, which the fixture then reads; dt
// runs as an ordinary user. The mount fails with DT_FAKE_MOUNT_FAIL as its
// message when that is set, and otherwise mounts at vers=DT_FAKE_VERS, or
// mounts nothing when that is empty. A umount without -f is busy or hangs
// when DT_FAKE_UMOUNT says so, and with stuck, every umount is busy. It
// returns a function giving the calls so far.
func fakeMounts(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	log, table := filepath.Join(dir, "log"), filepath.Join(dir, "mountinfo")
	scripts := map[string]string{
		"sudo": `[ "$1" = -n ] || exit 1; shift; echo "sudo -n $*" >> "$DT_FAKE_LOG"; exec "$@"`,
		"mount": `echo "mount $*" >> "$DT_FAKE_LOG"
for dir; do :; done
[ -z "$DT_FAKE_MOUNT_FAIL" ] || { echo "$DT_FAKE_MOUNT_FAIL" >&2; exit 32; }
[ -z "$DT_FAKE_VERS" ] || echo "600 28 0:58 / $dir rw shared:1 - nfs4 10.0.0.9:/export rw,vers=$DT_FAKE_VERS" >> "$DT_FAKE_MOUNTINFO"`,
		"umount": `echo "umount $*" >> "$DT_FAKE_LOG"
for dir; do :; done
[ -n "$DT_FAKE_DFS" ] && kill -0 "$DT_FAKE_DFS" 2>/dev/null && echo "dfs runs" >> "$DT_FAKE_LOG"
case "$1,$DT_FAKE_UMOUNT" in
-f,stuck | [!-]*,stuck | [!-]*,busy) echo "umount: $dir: target is busy" >&2; exit 32 ;;
[!-]*,hang) exec sleep 5 ;;
esac
grep -v " $dir " "$DT_FAKE_MOUNTINFO" > "$DT_FAKE_MOUNTINFO.new"; mv "$DT_FAKE_MOUNTINFO.new" "$DT_FAKE_MOUNTINFO"`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(table, []byte("22 1 259:2 / / rw shared:1 - ext4 /dev/root rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DT_FAKE_LOG", log)
	t.Setenv("DT_FAKE_MOUNTINFO", table)
	realInfo, realEuid, realWaits := mountinfo, geteuid, unmountWaits
	mountinfo, geteuid = table, func() int { return 1000 }
	unmountWaits = [2]time.Duration{time.Second, time.Second}
	t.Cleanup(func() { mountinfo, geteuid, unmountWaits = realInfo, realEuid, realWaits })
	return func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// Each NFS version mounts {server}:/export through sudo -n, with the posix
// setup's options for that version before the suite's own, on a short
// directory under /tmp that every user can reach; the kernel must then report
// the mount at that version. A failed mount, no mount, or a mount at another
// version fails the setup.
func TestMountShare(t *testing.T) {
	const suite = "noac,sync,lookupcache=none"
	for _, tc := range []struct {
		name, version, options string
		kernel, fail           string // what the fake mount reports, and its failure
		want, wantErr          string // the mount's options, or the error
	}{
		{"v3", "3", suite, "3", "", "nfsvers=3,tcp,port=40001,mountport=40001,nolock," + suite, ""},
		{"v4", "4", suite, "4.0", "", "vers=4.0,port=40001," + suite, ""},
		{"v4.1", "4.1", suite, "4.1", "", "vers=4.1,port=40001," + suite, ""},
		{"no options of the suite's", "4.1", "", "4.1", "", "vers=4.1,port=40001", ""},
		{"another version mounted", "4.1", "", "4.0", "", "", "is mounted with vers=4.0, not vers=4.1"},
		{"nothing mounted", "4.1", "", "", "", "", "mount exited 0, but nothing is mounted at"},
		{"mount fails", "3", "", "", "mount.nfs: Connection refused", "", "mount.nfs: Connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeMounts(t)
			t.Setenv("DT_FAKE_VERS", tc.kernel)
			t.Setenv("DT_FAKE_MOUNT_FAIL", tc.fail)
			var w bytes.Buffer
			s := &Server{nfsPort: 40001, out: &w}
			err := s.mountShare(context.Background(), "10.0.0.9", registry.NFS{Version: tc.version, Options: tc.options})
			t.Cleanup(func() { _ = os.Remove(s.mount) })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("got %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mount := "mount -t nfs -o " + tc.want + " 10.0.0.9:/export " + s.mount
			if got := calls(); len(got) != 2 || got[0] != "sudo -n "+mount || got[1] != mount {
				t.Errorf("calls %q, want %q", got, mount)
			}
			if _, err := os.Stat(s.mount); err != nil || filepath.Dir(s.mount) != "/tmp" || len(s.mount) > len("/tmp/dittofs-test") {
				t.Errorf("{mount} %s: %v", s.mount, err)
			}
		})
	}
}

// dt running as root, as in dtc, mounts without sudo.
func TestMountShareAsRoot(t *testing.T) {
	calls := fakeMounts(t)
	geteuid = func() int { return 0 }
	t.Setenv("DT_FAKE_VERS", "4.1")
	s := &Server{nfsPort: 40001, out: &bytes.Buffer{}}
	if err := s.mountShare(context.Background(), "10.0.0.9", registry.NFS{Version: "4.1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(s.mount) })
	if got := calls(); len(got) != 1 || !strings.HasPrefix(got[0], "mount ") {
		t.Errorf("calls %q", got)
	}
}

// The unmount tries a plain umount, then a forced lazy one when that fails or
// hangs, and removes {mount} once nothing is mounted there; a share that
// stays mounted keeps its directory, and dt says so.
func TestUnmount(t *testing.T) {
	for _, tc := range []struct {
		name, umount string
		forced       bool // whether it comes to umount -f -l
		stays        bool
	}{
		{"unmounts", "", false, false},
		{"busy", "busy", true, false},
		{"hangs", "hang", true, false},
		{"stuck", "stuck", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeMounts(t)
			t.Setenv("DT_FAKE_VERS", "4.1")
			t.Setenv("DT_FAKE_UMOUNT", tc.umount)
			var w bytes.Buffer
			s := &Server{nfsPort: 40001, out: &w}
			if err := s.mountShare(context.Background(), "10.0.0.9", registry.NFS{Version: "4.1"}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(s.mount) })
			start := time.Now()
			s.unmount()
			got := strings.Join(calls(), "\n")
			if forced := strings.Contains(got, "umount -f -l "+s.mount); forced != tc.forced || !strings.Contains(got, "sudo -n umount "+s.mount) || time.Since(start) > 4*time.Second {
				t.Errorf("after %s, calls:\n%s", time.Since(start), got)
			}
			_, err := os.Stat(s.mount)
			if stays := strings.Contains(w.String(), "the share stays mounted at "+s.mount); stays != tc.stays || os.IsNotExist(err) == tc.stays {
				t.Errorf("directory: %v; dt's lines:\n%s", err, w.String())
			}
		})
	}
}

// Stop unmounts the share while dfs still runs, since an unmount waits on a
// server that is gone, and then stops dfs.
func TestStopUnmountsFirst(t *testing.T) {
	calls := fakeMounts(t)
	t.Setenv("DT_FAKE_VERS", "4.1")
	bin, _ := fakeDFS(t)
	cmd := exec.Command(filepath.Join(bin, "dfs"))
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "dfs.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cmd: cmd, exited: make(chan struct{}), log: log, data: t.TempDir(), nfsPort: 40001, out: &bytes.Buffer{}}
	go func() { s.err = cmd.Wait(); close(s.exited) }()
	if err := s.mountShare(context.Background(), "10.0.0.9", registry.NFS{Version: "4.1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(s.mount) })
	t.Setenv("DT_FAKE_DFS", strconv.Itoa(cmd.Process.Pid))
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := calls(); !strings.Contains(strings.Join(got, "\n"), "umount "+s.mount+"\ndfs runs") || alive(cmd.Process.Pid) {
		t.Errorf("calls %q; dfs runs after Stop: %t", got, alive(cmd.Process.Pid))
	}
}
