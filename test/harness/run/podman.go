package run

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strings"
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
// which docker creates owned by root. The store volume takes its owner from
// the image's own directory.
func preparePodman(ctx context.Context, dir string, env []string, w io.Writer) error {
	if err := pull(ctx, podmanImage, dir, env, w); err != nil {
		return err
	}
	chown := []string{"docker", "run", "--rm", "-v", podmanCache + ":/cache", podmanImage, "chown", "podman:podman", "/cache"}
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
