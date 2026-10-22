//go:build !windows

package fixture

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeKnfsd puts on PATH a sudo that logs and runs its command, and a mount,
// exportfs, rpc.nfsd and rpc.mountd that succeed, unless DT_FAKE_FAIL names
// the program, and a umount that empties the directory it is given, as
// unmounting the tmpfs would. With DT_FAKE_HANG set, rpc.nfsd waits 3
// seconds before it starts the server, and logs that start. The kernel's
// settings are files in a directory of their own, with lease and grace
// times of 90 and the grace period on; rpc.nfsd writes the times it is
// given there, lists the versions DT_FAKE_VERSIONS names, NFSv4.0, 4.1 and
// 4.2 by default, and ends the grace period a second later. rpc.mountd runs
// until SIGTERM, and logs it. dt runs as an ordinary user, and makes its
// directories in one of the test's. It returns the settings' directory, and
// a function giving the calls so far.
func fakeKnfsd(t *testing.T) (nfsd string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	nfsd = filepath.Join(dir, "nfsd")
	if err := os.Mkdir(nfsd, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"threads": "0", "nfsv4leasetime": "90", "nfsv4gracetime": "90", "v4_end_grace": "N", "versions": "+3 +4 +4.1 +4.2"} {
		if err := os.WriteFile(filepath.Join(nfsd, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fail := `[ "${0##*/}" != "$DT_FAKE_FAIL" ] || { echo "${0##*/} failed" >&2; exit 1; }`
	scripts := map[string]string{
		"sudo":     `[ "$1" = -n ] || exit 1; shift; echo "$*" >> "$DT_FAKE_LOG"; exec "$@"`,
		"mount":    fail,
		"exportfs": fail,
		"umount":   `rm -rf "$1"/*`,
		"rpc.mountd": fail + `
trap 'echo "rpc.mountd stopped" >> "$DT_FAKE_LOG"; exit 0' TERM
while :; do sleep 0.1; done`,
		"rpc.nfsd": fail + `
[ -z "$DT_FAKE_HANG" ] || [ "$1" = 0 ] || { sleep 3; echo "rpc.nfsd starts the server" >> "$DT_FAKE_LOG"; }
while [ $# -gt 1 ]; do
	case "$1" in
	--lease-time) echo "$2" > "$DT_FAKE_NFSD/nfsv4leasetime" ;;
	--grace-time) echo "$2" > "$DT_FAKE_NFSD/nfsv4gracetime" ;;
	esac
	shift
done
if [ "$1" != 0 ]; then
	echo "${DT_FAKE_VERSIONS:--3 +4 +4.1 +4.2}" > "$DT_FAKE_NFSD/versions"
	(sleep 1; echo Y > "$DT_FAKE_NFSD/v4_end_grace") >/dev/null 2>&1 &
fi`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "log")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DT_FAKE_LOG", log)
	t.Setenv("DT_FAKE_NFSD", nfsd)
	t.Setenv("TMPDIR", t.TempDir())
	realDir, realEuid := nfsdDir, geteuid
	nfsdDir, geteuid = nfsd, func() int { return 1000 }
	t.Cleanup(func() { nfsdDir, geteuid = realDir, realEuid })
	return nfsd, func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// listening sets knfsdPort to a port that answers when up is set, and to one
// nothing listens on otherwise.
func listening(t *testing.T, up bool) {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	real := knfsdPort
	knfsdPort = l.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { knfsdPort = real; _ = l.Close() })
	if !up {
		_ = l.Close()
	}
}

// The kernel's server exports a new tmpfs as its NFSv4 root to {server}
// alone, to clients on any port, with /export in it that every user may
// write, and with root unsquashed for squash: none. Its lease and grace
// times are read first; rpc.mountd starts beside the server, which serves
// NFSv4.0, 4.1 and 4.2 alone over TCP on its port, with a 10-second grace
// period and the fixture's lease. The kernel's list of versions is read
// then, and the setup returns once the grace period has ended and the
// server answers. Stop stops the server, then rpc.mountd, removes the
// export, restores the times, and unmounts and removes the tmpfs. The nfsd
// file system is mounted first where it isn't yet.
func TestKnfsd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       registry.Fixture
		mounted bool // the nfsd file system is mounted already
		opts    string
		nfsd    string // rpc.nfsd's options after its port
		lease   string // the lease time while it runs
	}{
		{"squash none, lease 30s", registry.Fixture{Server: "knfsd", NFS: registry.NFS{Version: "export"}, Squash: "none", Lease: "30s"}, true,
			"rw,insecure,no_subtree_check,fsid=0,no_root_squash", "--no-nfs-version 3 --nfs-version 4.0 --nfs-version 4.1 --nfs-version 4.2 --no-udp --grace-time 10 --lease-time 30", "30"},
		{"the defaults", registry.Fixture{Server: "knfsd", NFS: registry.NFS{Version: "export"}}, false,
			"rw,insecure,no_subtree_check,fsid=0", "--no-nfs-version 3 --nfs-version 4.0 --nfs-version 4.1 --nfs-version 4.2 --no-udp --grace-time 10", "90"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nfsd, calls := fakeKnfsd(t)
			if !tc.mounted {
				_ = os.Remove(filepath.Join(nfsd, "threads"))
			}
			listening(t, true)
			out := t.TempDir()
			var w bytes.Buffer
			k, err := StartKnfsd(context.Background(), tc.f, out, &w)
			if err != nil {
				t.Fatalf("StartKnfsd: %v\n%s", err, w.String())
			}
			setup := calls()
			dir := strings.TrimPrefix(setup[0], "mount -t tmpfs -o mode=0777 tmpfs ")
			server, port := k.Placeholders()["server"], strconv.Itoa(knfsdPort)
			times := filepath.Join(nfsd, "nfsv4leasetime") + " " + filepath.Join(nfsd, "nfsv4gracetime")
			want := []string{"mount -t tmpfs -o mode=0777 tmpfs " + dir}
			if !tc.mounted {
				want = append(want, "mount -t nfsd nfsd "+nfsd)
			}
			want = append(want, "cat "+times,
				"exportfs -i -o "+tc.opts+" "+server+":"+dir,
				"rpc.nfsd --port "+port+" "+tc.nfsd,
				"cat "+filepath.Join(nfsd, "versions"))
			// rpc.mountd starts aside, so its line may come anywhere after the
			// export's: the server doesn't wait for it, since a request that
			// comes first waits for rpc.mountd's answer.
			mountd := "rpc.mountd --foreground --no-nfs-version 2 --no-nfs-version 3"
			if i := slices.Index(setup, mountd); i < len(want)-2 || !slices.Equal(slices.Delete(slices.Clone(setup), i, i+1), want) {
				t.Errorf("setup\n%s\nwant\n%s\nwith %s after the export", strings.Join(setup, "\n"), strings.Join(want, "\n"), mountd)
			}
			if got := k.Placeholders(); len(got) != 2 || got["nfs-port"] != port || net.ParseIP(server) == nil {
				t.Errorf("placeholders %v", got)
			}
			if fi, err := os.Stat(filepath.Join(dir, Share)); err != nil || fi.Mode().Perm() != 0o777 {
				t.Errorf("the share in the export: %v, %v", fi, err)
			}
			for file, v := range map[string]string{"v4_end_grace": "Y", "nfsv4leasetime": tc.lease, "nfsv4gracetime": "10"} {
				if got, _ := os.ReadFile(filepath.Join(nfsd, file)); strings.TrimSpace(string(got)) != v {
					t.Errorf("while it runs, %s is %q, want %s", file, got, v)
				}
			}
			if err := k.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
			}
			want = []string{"rpc.nfsd 0", "rpc.mountd stopped", "exportfs -u " + server + ":" + dir,
				"tee " + filepath.Join(nfsd, "nfsv4leasetime"), "tee " + filepath.Join(nfsd, "nfsv4gracetime"), "umount " + dir}
			if got := calls()[len(setup):]; !slices.Equal(got, want) {
				t.Errorf("stop\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, file := range []string{"nfsv4leasetime", "nfsv4gracetime"} {
				if got, _ := os.ReadFile(filepath.Join(nfsd, file)); strings.TrimSpace(string(got)) != "90" {
					t.Errorf("after Stop, %s is %q, not the 90 it was", file, got)
				}
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the export's directory stays: %v", err)
			}
		})
	}
}

// A setup that fails undoes what it did, the last step first, and the step
// it failed in: a failed mount of the tmpfs is unmounted, a failed export
// leaves rpc.mountd and the server unstarted, a server that doesn't answer,
// or that serves no NFSv4.0, is stopped with the rest, and an rpc.nfsd the
// setup's limit ends is ended before the undoing, so it starts nothing
// after.
func TestKnfsdFails(t *testing.T) {
	f := registry.Fixture{Server: "knfsd", NFS: registry.NFS{Version: "export"}, Squash: "none", Lease: "30s"}
	for _, tc := range []struct {
		name, fail string
		hang, up   bool
		versions   string // the kernel's list once rpc.nfsd ran, when not the default
		err        string
		undo       []string // after the setup's calls, with DIR for the export's directory
	}{
		{"the tmpfs", "mount", false, true, "", "mount failed", []string{"umount DIR"}},
		{"the export", "exportfs", false, true, "", "exportfs failed", []string{"exportfs -u SERVER:DIR", "tee LEASE", "tee GRACE", "umount DIR"}},
		{"no answer", "", false, false, "", "knfsd doesn't answer", []string{"rpc.nfsd 0", "rpc.mountd stopped", "exportfs -u SERVER:DIR", "tee LEASE", "tee GRACE", "umount DIR"}},
		{"the kernel refuses 4.0", "", false, true, "-3 +4 -4.0 +4.1 +4.2", "doesn't serve NFSv4.0, 4.1 and 4.2", []string{"rpc.nfsd 0", "rpc.mountd stopped", "exportfs -u SERVER:DIR", "tee LEASE", "tee GRACE", "umount DIR"}},
		{"the limit ends during rpc.nfsd", "", true, true, "", "deadline exceeded", []string{"rpc.nfsd 0", "rpc.mountd stopped", "exportfs -u SERVER:DIR", "tee LEASE", "tee GRACE", "umount DIR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nfsd, calls := fakeKnfsd(t)
			t.Setenv("DT_FAKE_FAIL", tc.fail)
			if tc.hang {
				t.Setenv("DT_FAKE_HANG", "1")
			}
			if tc.versions != "" {
				t.Setenv("DT_FAKE_VERSIONS", tc.versions)
			}
			listening(t, tc.up)
			limit := 10 * time.Second
			if tc.hang {
				limit = time.Second // past the steps before rpc.nfsd, short of its 3 seconds
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			var w bytes.Buffer
			start := time.Now()
			_, err := StartKnfsd(ctx, f, t.TempDir(), &w)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("StartKnfsd: %v, want %q\n%s", err, tc.err, w.String())
			}
			if tc.hang { // past rpc.nfsd's 3 seconds, so a start it went on with is in the calls
				time.Sleep(time.Until(start.Add(4 * time.Second)))
			}
			got := calls()
			dir := strings.TrimPrefix(got[0], "mount -t tmpfs -o mode=0777 tmpfs ")
			server, _ := address()
			r := strings.NewReplacer("DIR", dir, "SERVER", server, "LEASE", filepath.Join(nfsd, "nfsv4leasetime"), "GRACE", filepath.Join(nfsd, "nfsv4gracetime"))
			var want []string
			for _, c := range tc.undo {
				want = append(want, r.Replace(c))
			}
			if len(got) < len(want) || !slices.Equal(got[len(got)-len(want):], want) || slices.Contains(got[:len(got)-len(want)], "rpc.nfsd 0") {
				t.Errorf("calls\n%s\nwant them to end with\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if tc.fail == "exportfs" && slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "rpc.") }) {
				t.Errorf("rpc.mountd or rpc.nfsd started after the export failed:\n%s", strings.Join(got, "\n"))
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the export's directory stays: %v", err)
			}
		})
	}
}
