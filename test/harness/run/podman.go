package run

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// podmanImage runs the commands of a cell that needs podman on a machine
// whose own podman is missing or older than the scenarios need, as the
// scenarios' README runs setup.sh on a laptop. It is pinned, so a new podman
// can't change a run by itself.
//
// decision: the container is privileged, which the README keeps to a
// single-user VM. A shared host meets the need with its own podman, and dt
// then never starts this container.
const podmanImage = "quay.io/podman/stable:v5.8.9"

// The volumes that keep podman's images and the scenarios' cache from one
// test to the next, named as in the README, so a run there and one through dt
// share them.
const (
	podmanStore = "dittofs-podman-store"
	podmanCache = "dittofs-scenarios-cache"
)

// preparePodman pulls podman's image and gives its user the cache volume,
// which docker creates owned by root, and all in it, since a run by hand
// shares it and can have left root's directories there. The store volume
// takes its owner from the image's own directory.
func preparePodman(ctx context.Context, dir string, env []string, w io.Writer) error {
	if err := pull(ctx, podmanImage, dir, env, w); err != nil {
		return err
	}
	chown := []string{"docker", "run", "--rm", "-v", podmanCache + ":/cache", podmanImage, "chown", "-R", "podman:podman", "/cache"}
	shown := strings.Join(chown, " ")
	_, _ = fmt.Fprintf(w, "[dt] $ %s\n", shown)
	code, err := execute(ctx, chown, dir, env, w, false)
	if err == nil && code != 0 {
		err = fmt.Errorf("`%s` exited %d", shown, code)
	}
	return err
}

// inPodman returns argv run in podman's image, named so dt can remove the
// container once it kills its docker client, as the README runs setup.sh:
// as the image's user podman, with podman's system configuration alone,
// since the user's own mounts the outer /proc into every container, and the
// checkout at /src/dittofs, which docker mounts whatever the permissions of
// the directories above it. The cache is where setup.sh keeps it by default.
// The suite's variables are named as an image's are.
func (t tester) inPodman(argv []string) ([]string, string) {
	name := containerName(t.cell.Name)
	rel, _ := filepath.Rel(t.root, t.dir)
	docker := []string{"docker", "run", "--rm", "--name", name, "--privileged", "--user", "podman",
		"-e", "CONTAINERS_CONF=/etc/containers/containers.conf",
		"-v", t.root + ":/src/dittofs", "-w", path.Join("/src/dittofs", filepath.ToSlash(rel)),
		"-v", podmanStore + ":/home/podman/.local/share/containers",
		"-v", podmanCache + ":/var/tmp/dittofs-scenarios"}
	for _, kv := range t.own {
		k, _, _ := strings.Cut(kv, "=")
		docker = append(docker, "-e", k)
	}
	return slices.Concat(docker, []string{podmanImage}, argv), name
}

// logTail is how much of each file of a failed scenario's logs dt keeps, from
// its end. A variable, so the tests can shorten it.
//
// decision: the cap is per file, since setup.sh keeps a fixed handful of them:
// the scenario's trace, its timings and the server's and the S3's logs.
var logTail int64 = 4 << 20

// keepLogs copies the logs of a scenario that failed in podman's image out of
// the cache volume into the cell's results, failed/SCENARIO/, the last logTail
// bytes of each file; a retry that fails again keeps its own beside them, in
// failed/SCENARIO.2/, so the two runs don't mix. setup.sh keeps them in
// failed/SCENARIO/ once the scenario ends; one stopped at its limit never got
// there, and has those of its run so far in run/. It first prints how much
// disk is free where the volumes are, which, with the scenario's container
// gone, is what it had to start with: a full disk is the first suspect of one
// that writes gigabytes. It has a minute of its own, which an interrupt
// doesn't cut short, as removing a container has: the logs of the failure just
// seen are kept.
//
// decision: a host whose own podman runs setup.sh keeps the logs in its own
// cache, whose path setup.sh prints, and dt copies nothing there.
func (t tester) keepLogs(test string, timedOut bool) {
	scenario := path.Base(test)
	src := path.Join("failed", scenario)
	if timedOut {
		src = "run"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	argv := []string{"docker", "run", "--rm", "-v", podmanCache + ":/cache:ro", "-w", "/cache", podmanImage,
		"sh", "-c", `df -h . >&2 && tar -c -C "$0" .`, src}
	_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stderr = t.root, t.env, t.out
	dst := filepath.Join(t.vals["out"], "failed", scenario)
	for n := 2; ; n++ {
		if _, err := os.Lstat(dst); err != nil {
			break
		}
		dst = filepath.Join(t.vals["out"], "failed", scenario+"."+strconv.Itoa(n))
	}
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err == nil {
		err = untail(stdout, dst, t.out)
		_, _ = io.Copy(io.Discard, stdout) // what's left after an error, so tar can exit
		err = errors.Join(err, cmd.Wait())
	}
	if err != nil {
		_, _ = fmt.Fprintf(t.out, "[dt] %s's logs: %v\n", scenario, err)
		return
	}
	_, _ = fmt.Fprintf(t.out, "[dt] %s's logs: %s\n", scenario, dst)
}

// untail writes each file of the tar stream r below dir, only its last logTail
// bytes when it's longer, and says which it cut.
func untail(r io.Reader, dir string, w io.Writer) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if h.Typeflag != tar.TypeReg || !filepath.IsLocal(name) {
			continue
		}
		if h.Size > logTail {
			if _, err := io.CopyN(io.Discard, tr, h.Size-logTail); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "[dt] kept the last %d of %d bytes of %s\n", logTail, h.Size, path.Clean(h.Name))
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
}
