package fixture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// mountVers is the NFS version the kernel mounts each of a fixture's
// versions at, by the name its vers= option gives it.
var mountVers = map[string]string{"3": "3", "4": "4.0", "4.1": "4.1"}

// mounts reports whether an nfs version mounts the share: export serves it
// without a mount.
func mounts(version string) bool { return version != "" && version != "export" }

// mountOptions are the options an NFS mount at version takes from the server
// on port, before the suite's own, extra. NFSv3 reaches the MOUNT protocol on
// the same port, and nolock answers its locks in the client, which has no
// lock manager the server can reach.
func mountOptions(version string, port int, extra string) string {
	opts := fmt.Sprintf("vers=%s,port=%d", mountVers[version], port)
	if version == "3" {
		opts = fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,nolock", port, port)
	}
	if extra != "" {
		opts += "," + extra
	}
	return opts
}

// mountShare mounts the share from server over NFS, at nfs's version and
// with its options, on a new directory, {mount}, and checks that the kernel
// has it mounted there at that version.
func (s *Server) mountShare(ctx context.Context, server string, nfs registry.NFS) error {
	// {mount} is no longer than the posix setup's /tmp/dittofs-test, so a
	// test of path length measures the server rather than the mount point.
	// decision: it is under /tmp, not the temporary directory the
	// environment names, which can be long and sit under a directory
	// private to its owner, so a suite's users couldn't walk to the share.
	dir, err := os.MkdirTemp("/tmp", "dt")
	if err != nil {
		return err
	}
	s.mount = dir
	source := server + ":" + Share
	argv := asRoot("mount", "-t", "nfs", "-o", mountOptions(nfs.Version, s.nfsPort, nfs.Options), source, dir)
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	if err := runFor(ctx, 0, argv); err != nil {
		return fmt.Errorf("mounting %s at %s: %w", source, dir, err)
	}
	opts, mounted, err := mountedAt(dir)
	want := mountVers[nfs.Version]
	switch {
	case err != nil:
		return err
	case !mounted:
		return fmt.Errorf("mount exited 0, but nothing is mounted at %s", dir)
	case nfsVers(opts) != want:
		return fmt.Errorf("%s is mounted with vers=%s, not vers=%s", dir, nfsVers(opts), want)
	}
	return nil
}

// unmountWaits are how long an unmount waits for umount, and then for a
// forced lazy detach; the tests shorten them.
var unmountWaits = [2]time.Duration{15 * time.Second, 10 * time.Second}

// unmount unmounts {mount} and removes its directory, as the e2e tests'
// unmount does, never waiting long: a umount that a dead server holds up is
// left to end on its own, and a forced lazy detach takes the mount out of the
// namespace at once. A share that stays mounted keeps its directory, and dt
// says so.
func (s *Server) unmount() {
	if _, mounted, err := mountedAt(s.mount); mounted || err != nil {
		// decision: a umount that fails, as on a share a process still
		// uses, is forced as one that hangs is, since the server stops next.
		if runFor(context.Background(), unmountWaits[0], asRoot("umount", s.mount)) != nil {
			_ = runFor(context.Background(), unmountWaits[1], asRoot("umount", "-f", "-l", s.mount))
		}
		if _, mounted, err = mountedAt(s.mount); mounted || err != nil {
			_, _ = fmt.Fprintf(s.out, "[dt] fixture: the share stays mounted at %s; unmount it with: sudo umount -f -l %s\n", s.mount, s.mount)
			return
		}
	}
	_ = os.Remove(s.mount)
}

// geteuid is the user dt runs as; the tests replace it.
var geteuid = os.Geteuid

// asRoot is argv run as root: through sudo -n, which fails rather than ask
// for a password, unless dt runs as root already, as in dtc.
func asRoot(argv ...string) []string {
	if geteuid() == 0 {
		return argv
	}
	return append([]string{"sudo", "-n"}, argv...)
}

// runFor runs argv and waits for it until ctx ends or, when limit isn't 0,
// for limit. A command still running then is left to end on its own, since a
// mount or umount held up by its server can't be killed. Its error carries
// what the command printed.
func runFor(ctx context.Context, limit time.Duration, argv []string) error {
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// mountinfo is the kernel's table of this process's mounts; the tests
// replace it.
var mountinfo = "/proc/self/mountinfo"

// mountedAt reads mountinfo for the last mount at dir, and returns its file
// system's options and whether there is one.
func mountedAt(dir string) (opts string, mounted bool, err error) {
	data, err := os.ReadFile(mountinfo)
	if err != nil {
		return "", false, err
	}
	opts, mounted = lastMount(string(data), dir)
	return opts, mounted, nil
}

// lastMount finds the last mount at dir in a mountinfo table, which hides
// any mount at dir before it, and returns its file system's options. A line
// gives the mount point as its fifth field, then optional fields up to a
// lone -, and after it the file system's type, source and options.
//
// decision: dir is compared as mountinfo writes it, with no unescaping: the
// mount points dt makes hold no space or other character mountinfo escapes.
func lastMount(table, dir string) (opts string, mounted bool) {
	for _, line := range strings.Split(table, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		mount, fs := strings.Fields(before), strings.Fields(after)
		if ok && len(mount) >= 5 && len(fs) >= 3 && mount[4] == dir {
			opts, mounted = fs[2], true
		}
	}
	return opts, mounted
}

// nfsVers is the vers= option among a mount's options, or "".
func nfsVers(opts string) string {
	for _, o := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(o, "vers="); ok {
			return v
		}
	}
	return ""
}
