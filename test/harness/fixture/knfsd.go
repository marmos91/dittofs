package fixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// The kernel's NFS server takes its settings in nfsdDir, and a knfsd fixture
// serves on knfsdPort, its {nfs-port}; the tests replace both.
var (
	nfsdDir   = "/proc/fs/nfsd"
	knfsdPort = 2049
)

// Knfsd is a running knfsd fixture: the kernel's NFS server.
type Knfsd struct {
	vals map[string]string
	undo []func() // each done step's undoing, which Stop runs last first
	stop sync.Once
}

// StartKnfsd starts the kernel's NFS server, NFSv4 only, on knfsdPort, with
// f's lease. It exports a new tmpfs to {server} alone, with f's squash, as
// the server's root, which holds Share; rpc.mountd, which answers the
// kernel's questions about the export, writes to out/mountd.log. It waits
// for the server's grace period to end, and checks that it answers at
// {server}. It makes one attempt: when a step fails, or ctx ends first, it
// undoes what it did and returns why. w gets dt's lines on the setup.
func StartKnfsd(ctx context.Context, f registry.Fixture, out string, w io.Writer) (_ *Knfsd, err error) {
	server, err := address()
	if err != nil {
		return nil, err
	}
	// decision: the grace period is the least the kernel takes, 10 seconds,
	// and the setup waits it out. The server refuses new state while it
	// lasts, and without nfsdcld, which the system's own service starts, it
	// can't know that no client has state to reclaim, and skip it.
	//
	// The minor versions are named, so the host's nfs.conf can't turn off
	// one that pynfs tests; rpc.nfsd still reads the rest of that file, and
	// a listener it adds there, such as RDMA's, is the host's.
	nfsd := []string{"rpc.nfsd", "--port", strconv.Itoa(knfsdPort), "--no-nfs-version", "3",
		"--nfs-version", "4.0", "--nfs-version", "4.1", "--nfs-version", "4.2", "--no-udp", "--grace-time", "10"}
	settings, err := nfsSettings(f) // the lease, read as a dfs fixture's
	if err != nil {
		return nil, err
	}
	if settings != nil && settings.LeaseTime != nil {
		nfsd = append(nfsd, "--lease-time", strconv.Itoa(*settings.LeaseTime))
	}
	// ponytail: a dt that is killed leaves the server, rpc.mountd and the
	// tmpfs behind, and the next knfsd cell finds its port taken and doesn't
	// run. Have the fixture remove what a dead dt left, as the namespaced
	// clients do, if that shows up.
	k := &Knfsd{}
	defer func() {
		if err != nil {
			_ = k.Stop()
		}
	}()
	dir, err := os.MkdirTemp("", "dt-knfsd-")
	if err != nil {
		return nil, err
	}
	k.undo = append(k.undo, func() {
		if err := os.Remove(dir); err != nil {
			_, _ = fmt.Fprintf(w, "[dt] fixture: %v; unmount it with: sudo umount %s\n", err, dir)
		}
	})
	// A step's undoing is added before the step: a step that ctx ends may
	// have done part of its work, and undoing one that didn't happen does
	// nothing.
	//
	// decision: the export is a tmpfs, which the kernel exports wherever dt
	// runs, while it refuses the overlay file system a container's /tmp is.
	k.undo = append(k.undo, func() { _ = runFor(context.Background(), 0, asRoot("umount", dir)) })
	if err := waitStep(ctx, w, asRoot("mount", "-t", "tmpfs", "-o", "mode=0777", "tmpfs", dir)); err != nil {
		return nil, err
	}
	share := filepath.Join(dir, Share)
	if err := os.Mkdir(share, 0o777); err != nil {
		return nil, err
	}
	if err := os.Chmod(share, 0o777); err != nil { // past the umask
		return nil, err
	}

	// rpc.nfsd sets the lease and grace times for every server on this
	// network stack, so they are read first and restored once it stops.
	if _, err := os.Stat(filepath.Join(nfsdDir, "threads")); err != nil {
		if err := waitStep(ctx, w, asRoot("mount", "-t", "nfsd", "nfsd", nfsdDir)); err != nil {
			return nil, err
		}
	}
	times := []string{filepath.Join(nfsdDir, "nfsv4leasetime"), filepath.Join(nfsdDir, "nfsv4gracetime")}
	cat := asRoot(append([]string{"cat"}, times...)...)
	got, err := exec.CommandContext(ctx, cat[0], cat[1:]...).Output()
	old := strings.Fields(string(got))
	if err != nil || len(old) != len(times) {
		return nil, fmt.Errorf("reading the server's lease and grace times: %q, %v", got, err)
	}
	k.undo = append(k.undo, func() {
		for i, file := range times {
			tee := asRoot("tee", file)
			cmd := exec.Command(tee[0], tee[1:]...)
			cmd.Stdin = strings.NewReader(old[i])
			_ = cmd.Run()
		}
	})

	// insecure: pynfs connects from a port above 1023. fsid=0: the export is
	// the server's NFSv4 root.
	opts := "rw,insecure,no_subtree_check,fsid=0"
	if f.Squash == "none" {
		opts += ",no_root_squash"
	}
	export := server + ":" + dir
	k.undo = append(k.undo, func() { _ = runFor(context.Background(), 0, asRoot("exportfs", "-u", export)) })
	if err := waitStep(ctx, w, asRoot("exportfs", "-i", "-o", opts, export)); err != nil {
		return nil, err
	}

	log, err := os.Create(filepath.Join(out, "mountd.log"))
	if err != nil {
		return nil, err
	}
	argv := asRoot("rpc.mountd", "--foreground", "--no-nfs-version", "2", "--no-nfs-version", "3")
	_, _ = fmt.Fprintf(w, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	mountd := exec.Command(argv[0], argv[1:]...)
	mountd.Stdout, mountd.Stderr = log, log
	ownGroup(mountd)
	err = mountd.Start()
	_ = log.Close() // mountd holds its own
	if err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = mountd.Wait(); close(exited) }()
	k.undo = append(k.undo, func() {
		stopGroup(mountd, false)
		select {
		case <-exited:
		case <-time.After(stopGrace):
			stopGroup(mountd, true)
			<-exited
		}
	})

	k.undo = append(k.undo, func() { _ = runFor(context.Background(), 0, asRoot("rpc.nfsd", "0")) })
	if err := waitStep(ctx, w, asRoot(nfsd...)); err != nil {
		return nil, err
	}
	// rpc.nfsd only logs a version the kernel refuses, so the kernel's list
	// is read: it gives 4.0 as -4.0 when it is off, and not at all when it
	// is on.
	cat = asRoot("cat", filepath.Join(nfsdDir, "versions"))
	vers, err := exec.CommandContext(ctx, cat[0], cat[1:]...).Output()
	on := strings.Fields(string(vers))
	if err != nil || slices.Contains(on, "-4.0") || !slices.Contains(on, "+4.1") || !slices.Contains(on, "+4.2") {
		return nil, fmt.Errorf("knfsd doesn't serve NFSv4.0, 4.1 and 4.2: %q, %v", vers, err)
	}
	for {
		state, err := os.ReadFile(filepath.Join(nfsdDir, "v4_end_grace"))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(state)) == "Y" {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("knfsd's grace period didn't end: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(server, strconv.Itoa(knfsdPort)), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("knfsd doesn't answer: %w", err)
	}
	_ = conn.Close()
	k.vals = map[string]string{"server": server, "nfs-port": strconv.Itoa(knfsdPort)}
	_, _ = fmt.Fprintf(w, "[dt] fixture: knfsd exports %s at %s:%d\n", Share, server, knfsdPort)
	return k, nil
}

// Placeholders returns what the server gives a cell's commands, by name.
func (k *Knfsd) Placeholders() map[string]string { return k.vals }

// Reset fails: a knfsd fixture has one share, and no reset.
func (k *Knfsd) Reset(string) error { return errors.New("a knfsd fixture resets no share") }

// Command is argv as it is: the tests run on this machine.
func (k *Knfsd) Command(argv []string, _ string, _ []string) ([]string, error) { return argv, nil }

// Stop undoes each step StartKnfsd did, the last first, whatever the ones
// before did: the server stops, rpc.mountd ends, the export goes, the lease
// and grace times are restored, and the tmpfs is unmounted and its directory
// removed. The nfsd file system stays mounted, as rpc.nfsd would leave it.
func (k *Knfsd) Stop() error {
	k.stop.Do(func() {
		for i := len(k.undo) - 1; i >= 0; i-- {
			k.undo[i]()
		}
	})
	return nil
}

// waitStep is step for a command that has to end before its undoing runs:
// an rpc.nfsd left going would start the server after Stop stopped it. When
// ctx ends first, the command's group gets SIGTERM, which sudo relays, and
// waitStep waits for the command to end.
//
// decision: that wait has no limit, since SIGKILL would end sudo alone and
// leave its command going.
func waitStep(ctx context.Context, w io.Writer, argv []string) error {
	_, _ = fmt.Fprintf(w, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	ownGroup(cmd)
	cmd.Cancel = func() error { stopGroup(cmd, false); return nil }
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case err != nil:
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	default:
		return nil
	}
	return fmt.Errorf("`%s`: %w", strings.Join(argv, " "), err)
}
