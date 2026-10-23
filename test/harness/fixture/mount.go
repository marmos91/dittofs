package fixture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// mountVers is the NFS version the kernel mounts each of a fixture's
// versions at, by the name its vers= option gives it.
var mountVers = map[string]string{"3": "3", "4": "4.0", "4.1": "4.1"}

// mountOptions are the options a mount of nfs takes from the server on port:
// the version, the port, the suite's own options last. A mount whose version
// its options give takes them first, as they are. NFSv3 reaches the MOUNT
// protocol on the same port, and nolock answers its locks in the client,
// which has no lock manager the server can reach, unless it's a namespaced
// client, which has its own.
func mountOptions(nfs registry.NFS, port int, namespaced bool) string {
	vers, opts := mountVers[nfs.Version], []string{}
	switch {
	case nfs.FromOptions:
		vers, opts = versOf(nfs.Options), append(opts, nfs.Options)
	case vers == "3":
		opts = append(opts, "nfsvers=3", "tcp")
	default:
		opts = append(opts, "vers="+vers)
	}
	opts = append(opts, fmt.Sprintf("port=%d", port))
	if vers == "3" {
		opts = append(opts, fmt.Sprintf("mountport=%d", port))
		if !namespaced {
			opts = append(opts, "nolock")
		}
	}
	if !nfs.FromOptions {
		opts = append(opts, nfs.Options)
	}
	return strings.Join(slices.DeleteFunc(opts, func(o string) bool { return o == "" }), ",")
}

// wantVers is the vers= the kernel reports for a mount of nfs: its version's,
// or the one its options give with a minor version or as 3. "" asks nothing:
// the kernel negotiates it.
func wantVers(nfs registry.NFS) string {
	if !nfs.FromOptions {
		return mountVers[nfs.Version]
	}
	if v := versOf(nfs.Options); v == "3" || strings.Contains(v, ".") {
		return v
	}
	return ""
}

// mountShare mounts the share from server over NFS, at nfs's version and
// with its options, on a new directory, {mount}, and checks that the kernel
// has it mounted there at that version. With a namespaced client, the client
// mounts it.
func (s *Server) mountShare(ctx context.Context, server string, nfs registry.NFS) error {
	opts, err := s.mountAt(ctx, "nfs", mountOptions(nfs, s.nfsPort, s.clients != nil), server+":"+Share)
	if err != nil {
		return err
	}
	if want := wantVers(nfs); want != "" && versOf(opts) != want {
		return fmt.Errorf("%s is mounted with vers=%s, not vers=%s", s.mount, versOf(opts), want)
	}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: NFS vers=%s mounted at %s\n", versOf(opts), s.mount)
	return nil
}

// mountAt mounts source as root, with a file system of type fstype and
// options opts, on a new directory, {mount}, checks that the kernel has it
// mounted there, and returns the mount's options as the kernel reports them.
func (s *Server) mountAt(ctx context.Context, fstype, opts, source string) (string, error) {
	dir, err := mountDir()
	if err != nil {
		return "", err
	}
	s.mount = dir
	argv := s.root("mount", "-t", fstype, "-o", opts, source, dir)
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	if err := runFor(ctx, 0, argv); err != nil {
		return "", fmt.Errorf("mounting %s at %s: %w", source, dir, err)
	}
	got, mounted, err := s.mountedAt(dir)
	switch {
	case err != nil:
		return "", err
	case !mounted:
		return "", fmt.Errorf("mount exited 0, but nothing is mounted at %s", dir)
	}
	return got, nil
}

// mountDir makes {mount}'s directory. It is no longer than the posix setup's
// /tmp/dittofs-test, so a test of path length measures the server rather
// than the mount point.
//
// decision: it is under /tmp, not the temporary directory the environment
// names, which can be long and sit under a directory private to its owner,
// so a suite's users couldn't walk to the share.
func mountDir() (string, error) {
	return os.MkdirTemp("/tmp", "dt")
}

// root is argv run as root where the share is mounted: on the namespaced
// client, or on this machine.
func (s *Server) root(argv ...string) []string {
	if s.clients != nil {
		return s.clients.in(1, "", argv...)
	}
	return asRoot(argv...)
}

// mountedAt reads the mount table where the share is mounted for the last
// mount at dir, as the package's mountedAt does on this machine.
func (s *Server) mountedAt(dir string) (opts string, mounted bool, err error) {
	if s.clients == nil {
		return mountedAt(dir)
	}
	argv := s.root("cat", "/proc/self/mountinfo")
	table, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return "", false, fmt.Errorf("reading the client's mounts: %w", err)
	}
	opts, mounted = lastMount(string(table), dir)
	return opts, mounted, nil
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
	if goos == "darwin" || goos == "windows" { // where dt mounts over SMB alone, as the user
		s.unmountSMB()
		return
	}
	if _, mounted, err := s.mountedAt(s.mount); mounted || err != nil {
		// decision: a umount that fails, as on a share a process still
		// uses, is forced as one that hangs is, since the server stops next.
		if runFor(context.Background(), unmountWaits[0], s.root("umount", s.mount)) != nil {
			_ = runFor(context.Background(), unmountWaits[1], s.root("umount", "-f", "-l", s.mount))
		}
		if _, mounted, err = s.mountedAt(s.mount); mounted || err != nil {
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

// versOf is the vers= option among a mount's options, or "".
func versOf(opts string) string {
	for _, o := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(o, "vers="); ok {
			return v
		}
	}
	return ""
}
