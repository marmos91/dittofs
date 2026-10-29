package fixture

import (
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// The NFS version mounted at a directory is the vers= among the file
// system's options on the last line of mountinfo whose mount point is that
// directory, whatever optional fields come before the separator.
func TestMountedVersion(t *testing.T) {
	const (
		root  = "22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/root rw,discard\n"
		tmp   = "28 22 0:26 / /tmp rw,nosuid,nodev shared:5 - tmpfs tmpfs rw,size=8G\n"
		v3    = "612 28 0:58 / /tmp/dt42 rw,relatime shared:325 - nfs 10.0.0.9:/export rw,sync,relatime,vers=3,hard,nolock,proto=tcp,port=40001,mountaddr=10.0.0.9,mountvers=3,mountport=40001,local_lock=all\n"
		v41   = "613 28 0:59 / /tmp/dt42 rw,relatime - nfs4 10.0.0.9:/export rw,sync,relatime,vers=4.1,hard,proto=tcp,port=40001,clientaddr=10.0.0.9,local_lock=none\n"
		v40   = "614 28 0:60 / /tmp/dt42 rw,relatime shared:330 master:7 - nfs4 10.0.0.9:/export rw,sync,vers=4.0,proto=tcp,port=40001\n"
		other = "615 28 0:61 / /tmp/dt420 rw,relatime shared:331 - nfs4 10.0.0.9:/export rw,vers=4.1,port=40002\n"
		over  = "616 613 0:62 / /tmp/dt42 rw,relatime shared:332 - tmpfs tmpfs rw\n"
	)
	for _, tc := range []struct {
		name    string
		table   string
		vers    string
		mounted bool
	}{
		{"nothing mounted there", root + tmp, "", false},
		{"v3", root + tmp + v3, "3", true},
		{"v4.1, no optional field", root + tmp + v41, "4.1", true},
		{"v4.0, two optional fields", root + tmp + v40, "4.0", true},
		{"only a longer path", root + tmp + other, "", false},
		{"the last of two mounts", root + tmp + v3 + v41 + other, "4.1", true},
		{"hidden by a mount over it", root + tmp + v41 + over, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, mounted := lastMount(tc.table, "/tmp/dt42")
			if got := nfsVers(opts); got != tc.vers || mounted != tc.mounted {
				t.Errorf("vers %q, mounted %t; want %q, %t", got, mounted, tc.vers, tc.mounted)
			}
		})
	}
}

// A mount whose version its options give takes them first as they are, with
// MOUNT on the NFS port for NFSv3; a namespaced client's NFSv3 mount has no
// nolock, since its locks reach the server. The kernel must then report the
// version the options give, a minor one or 3, and with none any.
func TestMountOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nfs        registry.NFS
		namespaced bool
		opts, vers string
	}{
		{"v3", registry.NFS{Version: "3", Options: "noac"}, false, "nfsvers=3,tcp,port=40001,mountport=40001,nolock,noac", "3"},
		{"v3 from a namespaced client", registry.NFS{Version: "3"}, true, "nfsvers=3,tcp,port=40001,mountport=40001", "3"},
		{"v4", registry.NFS{Version: "4"}, false, "vers=4.0,port=40001", "4.0"},
		{"v3 from the options", registry.NFS{Options: "vers=3,tcp,mountproto=tcp", FromOptions: true}, true, "vers=3,tcp,mountproto=tcp,port=40001,mountport=40001", "3"},
		{"v3 from the options, on the server's stack", registry.NFS{Options: "vers=3", FromOptions: true}, false, "vers=3,port=40001,mountport=40001,nolock", "3"},
		{"v4.1 from the options", registry.NFS{Options: "vers=4.1,noac", FromOptions: true}, true, "vers=4.1,noac,port=40001", "4.1"},
		{"v4, its minor negotiated", registry.NFS{Options: "vers=4", FromOptions: true}, true, "vers=4,port=40001", ""},
		{"no options", registry.NFS{FromOptions: true}, true, "port=40001", ""},
	} {
		if got := mountOptions(tc.nfs, 40001, tc.namespaced); got != tc.opts {
			t.Errorf("%s: options %q, want %q", tc.name, got, tc.opts)
		}
		if got := wantVers(tc.nfs); got != tc.vers {
			t.Errorf("%s: vers %q, want %q", tc.name, got, tc.vers)
		}
	}
}
