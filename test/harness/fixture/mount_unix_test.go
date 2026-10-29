//go:build !windows

package fixture

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeMounts puts a sudo, a mount and a umount on PATH that log each call
// and keep a mountinfo table of their own, which the fixture then reads as
// the kernel's client's; dt runs as an ordinary user. The mount fails with
// DT_FAKE_MOUNT_FAIL as its message when that is set, and otherwise mounts at
// vers=DT_FAKE_VERS, or mounts nothing when that is empty. A umount without
// -f is busy or hangs when DT_FAKE_UMOUNT says so, and with stuck, every
// umount is busy. The other systems' SMB clients are there too: a
// mount_smbfs that logs and fails as the mount does, an smbutil that reports
// a share at DT_FAKE_VERS or none, a powershell that logs the share it maps
// and whether its command line holds the password, fails as the mount does,
// and prints drive Z: at DT_FAKE_VERS, and a net that logs. It returns a
// function giving the calls so far. The unmount keeps its own waits, which a
// fake that doesn't hang never reaches however slowly it starts; a test of a
// hang shortens them itself.
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
		"mount_smbfs": `echo "mount_smbfs $*" >> "$DT_FAKE_LOG"
[ -z "$DT_FAKE_MOUNT_FAIL" ] || { echo "$DT_FAKE_MOUNT_FAIL" >&2; exit 68; }`,
		"smbutil": `[ -z "$DT_FAKE_VERS" ] && echo '[]' || printf '[\n  {\n    "SERVER_NAME" : "10.0.0.9",\n    "share_name" : "smbbasic",\n    "SMB_VERSION" : "SMB_%s"\n  }\n]\n' "$DT_FAKE_VERS"`,
		"powershell": `printf 'powershell %s %s %s %s %s\n' "$DT_SMB_PATH" "$DT_SMB_PORT" "$DT_SMB_USER" "$DT_SMB_SERVER" "$DT_SMB_SHARE" >> "$DT_FAKE_LOG"
case "$*" in *"$DT_SMB_PASSWORD"*) echo "the password on its command line" >> "$DT_FAKE_LOG" ;; esac
[ -z "$DT_FAKE_MOUNT_FAIL" ] || { echo "$DT_FAKE_MOUNT_FAIL" >&2; exit 1; }
echo "Z: $DT_FAKE_VERS"`,
		"net": `echo "net $*" >> "$DT_FAKE_LOG"`,
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
	realInfo, realEuid, realWaits, realGOOS := mountinfo, geteuid, unmountWaits, goos
	mountinfo, geteuid, goos = table, func() int { return 1000 }, "linux"
	t.Cleanup(func() { mountinfo, geteuid, unmountWaits, goos = realInfo, realEuid, realWaits, realGOOS })
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

// Each system's own client mounts the first share, as {user}, on the
// server's SMB port, and must report SMB 3.1.1: mount.cifs through sudo -n,
// asked for that dialect, with the password in a credentials file only the
// user dt runs as reads; mount_smbfs as that user, with it in the share's
// URL, which dt's lines hide; New-SmbMapping with it in the environment, on
// a drive, then {mount}. A failed mount, no mount, or a mount at another
// dialect fails the setup; a drive that was mapped is {mount} even then, so
// Stop removes it.
func TestMountSMB(t *testing.T) {
	for _, tc := range []struct {
		name, goos, dialect, vers, fail string
		want, wantErr                   string // the client's call, {mount} and {data} filled in, or the error
	}{
		{"cifs", "linux", "", "3.1.1", "", "sudo -n mount -t cifs -o vers=3.1.1,port=40002,credentials={data}/smb-credentials //10.0.0.9/smbbasic {mount}", ""},
		{"cifs at another dialect", "linux", "", "3.0", "", "", "is mounted at SMB 3.0, not 3.1.1"},
		{"cifs mounts nothing", "linux", "", "", "", "", "mount exited 0, but nothing is mounted at"},
		{"cifs fails", "linux", "", "", "mount error(13): Permission denied", "", "mount error(13): Permission denied"},
		{"cifs at the fixture's dialect", "linux", "2.1", "2.1", "", "sudo -n mount -t cifs -o vers=2.1,port=40002,credentials={data}/smb-credentials //10.0.0.9/smbbasic {mount}", ""},
		{"cifs not at the fixture's dialect", "linux", "2.1", "3.1.1", "", "", "is mounted at SMB 3.1.1, not 2.1"},
		{"smbfs", "darwin", "", "3.1.1", "", "mount_smbfs -N -o nobrowse //dt-user:s3cret@10.0.0.9:40002/smbbasic {mount}", ""},
		{"smbfs at another dialect", "darwin", "", "3.0.2", "", "", "is mounted at SMB 3.0.2, not 3.1.1"},
		{"smbfs mounts nothing", "darwin", "", "", "", "", "smbutil statshares lists 0 shares mounted there"},
		{"smbfs fails", "darwin", "", "", "mount_smbfs: server rejected the connection: Authentication error", "", "Authentication error"},
		{"mapping", "windows", "", "3.1.1", "", `powershell \\10.0.0.9\smbbasic 40002 dt-user 10.0.0.9 smbbasic`, ""},
		{"mapping at another dialect", "windows", "", "3.0.2", "", "", "Z: is mounted at SMB 3.0.2, not 3.1.1"},
		{"mapping prints no dialect", "windows", "", "", "", "", "printed no drive and dialect"},
		{"mapping fails", "windows", "", "", "New-SmbMapping : The network path was not found.", "", "The network path was not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeMounts(t)
			goos = tc.goos
			t.Setenv("DT_FAKE_VERS", tc.vers)
			t.Setenv("DT_FAKE_MOUNT_FAIL", tc.fail)
			var w bytes.Buffer
			s := &Server{f: registry.Fixture{SMB: registry.SMB{Dialect: tc.dialect}}, smbPort: 40002, data: t.TempDir(), shares: []apiclient.CreateShareRequest{{Name: "/smbbasic"}, {Name: "/other"}}, out: &w}
			err := s.mountSMB(context.Background(), "10.0.0.9", "s3cret")
			if tc.goos != "windows" {
				t.Cleanup(func() { _ = os.Remove(s.mount) })
			}
			if strings.Contains(w.String(), "s3cret") || strings.Contains(fmt.Sprint(err), "s3cret") || slices.Contains(calls(), "the password on its command line") {
				t.Errorf("the password shows: %v\ndt's lines:\n%s\ncalls %q", err, w.String(), calls())
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("got %v, want %q", err, tc.wantErr)
				}
				if tc.goos == "windows" && tc.fail == "" && s.mount != "Z:" {
					t.Errorf("{mount} %q after a mapping that failed its check, want Z:", s.mount)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := strings.NewReplacer("{mount}", s.mount, "{data}", s.data).Replace(tc.want)
			if got := calls(); !slices.Contains(got, want) || slices.ContainsFunc(got, func(c string) bool { return tc.goos == "darwin" && strings.HasPrefix(c, "sudo") }) {
				t.Errorf("calls %q, want %q, and no sudo for mount_smbfs", got, want)
			}
			if creds, _ := os.ReadFile(filepath.Join(s.data, "smb-credentials")); tc.goos == "linux" && string(creds) != "username=dt-user\npassword=s3cret\n" {
				t.Errorf("credentials %q", creds)
			}
			if mount := map[string]string{"windows": "Z:"}[tc.goos]; mount != "" && s.mount != mount || !strings.Contains(w.String(), "SMB "+cmp.Or(tc.dialect, "3.1.1")+" mounted at "+s.mount+", as dt-user") {
				t.Errorf("{mount} %q; dt's lines:\n%s", s.mount, w.String())
			}
		})
	}
}

// A share mount_smbfs mounted is unmounted by the user dt runs as, forced
// when a plain umount fails, and its directory removed; one that stays
// mounted keeps it, and dt says so. A mapped drive is removed with net use.
func TestUnmountSMB(t *testing.T) {
	for _, tc := range []struct {
		name, goos, umount string
		want               []string // the calls, {mount} filled in
		stays              bool
	}{
		{"smbfs", "darwin", "", []string{"umount {mount}"}, false},
		{"smbfs busy", "darwin", "busy", []string{"umount {mount}", "umount -f {mount}"}, false},
		{"smbfs stuck", "darwin", "stuck", []string{"umount {mount}", "umount -f {mount}"}, true},
		{"drive", "windows", "", []string{"net use Z: /delete /y"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeMounts(t)
			goos = tc.goos
			t.Setenv("DT_FAKE_UMOUNT", tc.umount)
			var w bytes.Buffer
			s := &Server{out: &w, mount: "Z:"}
			if tc.goos == "darwin" {
				dir, err := mountDir()
				if err != nil {
					t.Fatal(err)
				}
				s.mount = dir
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				if tc.stays { // what a share still mounted shows there
					if err := os.WriteFile(filepath.Join(dir, "file.txt"), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.unmount()
			want := make([]string, len(tc.want))
			for i, c := range tc.want {
				want[i] = strings.ReplaceAll(c, "{mount}", s.mount)
			}
			if got := calls(); !slices.Equal(got, want) {
				t.Errorf("calls %q, want %q", got, want)
			}
			_, err := os.Stat(s.mount)
			if stays := strings.Contains(w.String(), "the share stays mounted at "+s.mount); tc.goos == "darwin" && (stays != tc.stays || os.IsNotExist(err) == tc.stays) {
				t.Errorf("directory: %v; dt's lines:\n%s", err, w.String())
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
			if tc.umount == "hang" {
				// the fake sleeps 5 s; shorter waits make unmount give up on it
				unmountWaits = [2]time.Duration{time.Second, time.Second}
			}
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

// With a namespaced client the client mounts the share, as dt does there:
// root's mount and umount enter its namespaces, and the check reads its
// mount table, not this machine's.
func TestMountShareOnClient(t *testing.T) {
	calls := fakeMounts(t)
	dir := t.TempDir()
	nsenter := `echo "nsenter $*" >> "$DT_FAKE_LOG"
while [ "$1" != -- ]; do shift; done; shift
[ "$*" = "cat /proc/self/mountinfo" ] && exec cat "$DT_FAKE_MOUNTINFO"
exec "$@"`
	if err := os.WriteFile(filepath.Join(dir, "nsenter"), []byte("#!/bin/sh\n"+nsenter+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mountinfo = "/nonexistent" // only the client's table holds the mount
	t.Setenv("DT_FAKE_VERS", "3")
	s := &Server{nfsPort: 40001, out: &bytes.Buffer{}, clients: &clients{bridge: "dtnfs7", dir: "/c"}}
	if err := s.mountShare(context.Background(), "10.51.7.1", registry.NFS{Options: "vers=3,tcp,mountproto=tcp", FromOptions: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(s.mount) })
	s.unmount()
	enter := "nsenter --net=/run/netns/dtnfs7-1 --mount=/c/ns/1.mnt --uts=/c/ns/1.uts -- "
	got := calls()
	for _, call := range []string{
		"sudo -n " + enter + "mount -t nfs -o vers=3,tcp,mountproto=tcp,port=40001,mountport=40001 10.51.7.1:/export " + s.mount,
		"sudo -n " + enter + "cat /proc/self/mountinfo",
		"sudo -n " + enter + "umount " + s.mount,
	} {
		if !slices.Contains(got, call) {
			t.Errorf("no %q in:\n%s", call, strings.Join(got, "\n"))
		}
	}
	if _, err := os.Stat(s.mount); !os.IsNotExist(err) {
		t.Errorf("{mount} stays: %v", err)
	}
}

// users: self runs the tests as the user dt runs as, which dt running as
// root can't give: the fixture doesn't start.
func TestSelfNeedsAnOrdinaryUser(t *testing.T) {
	realEuid := geteuid
	t.Cleanup(func() { geteuid = realEuid })
	geteuid = func() int { return 0 }
	_, err := Start(context.Background(), registry.Fixture{Profile: "memory", Users: []string{"self"}}, nil, t.TempDir(), t.TempDir(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "dt runs as root here") {
		t.Errorf("got %v", err)
	}
}
