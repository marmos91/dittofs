package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/fixture"
	"github.com/marmos91/dittofs/test/harness/report"
)

// notStarted are the exit statuses docker run gives when its container didn't
// start, and env when the command it runs as root didn't.
func notStarted(code int) bool { return code >= 125 && code <= 127 }

// notInName matches what a container's name can't hold.
var notInName = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// inImage returns argv run in the suite's image, as a container of its own
// with the suite's environment and volumes, on the network the fixture's
// server is on. It names the container, so dt can remove it once it kills
// the docker client that started it: killing the client leaves the container
// running.
func (t tester) inImage(argv []string) (docker []string, name string, err error) {
	u := t.cell.Unit
	name = containerName(t.cell.Name)
	// decision: the container runs as its image's own user, root in the
	// images the registry names, since an image's entrypoint may write paths
	// of its own that another user can't. An engine that keeps a file's owner
	// in a volume then leaves what the container writes there, such as WPTS's
	// results in {out}/config, owned by root, and only dtc hands it back to
	// the user (DT_FIX_OWNER). Pass --user once a suite's image is known to
	// run as any user.
	docker = []string{"docker", "run", "--rm", "--name", name, "--network", fixture.Network()}
	// decision: dir is given to docker as it fills, so a relative one is
	// docker's to refuse: it exits 125, and the cell is not run.
	if t.inside != "" {
		docker = append(docker, "-w", t.inside)
	}
	// decision: -e names each variable without its value, which docker takes
	// from its own environment, so values stay out of the arguments dt prints
	// and the process table shows.
	for _, kv := range t.own {
		k, _, _ := strings.Cut(kv, "=")
		docker = append(docker, "-e", k)
	}
	for _, host := range slices.Sorted(maps.Keys(u.Volumes)) {
		from, err := replace(host, t.vals, t.lists, nil)
		if err != nil {
			return nil, "", err
		}
		to, err := replace(u.Volumes[host], t.vals, t.lists, nil)
		if err != nil {
			return nil, "", err
		}
		// decision: a relative host path is taken from the repository
		// root, as commands are; docker would read it as a volume's name.
		if !filepath.IsAbs(from) {
			from = filepath.Join(t.root, from)
		}
		docker = append(docker, "-v", from+":"+to)
	}
	return append(append(docker, u.Image), argv...), name, nil
}

// containerName names a container of the cell's, unique to it.
func containerName(cell string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "dt-" + notInName.ReplaceAllString(report.Slug(cell), "-") + "-" + hex.EncodeToString(b)
}

// pull pulls image unless docker has it, before the cell's first test, so
// neither the pull's time nor a stalled registry counts against a test. It's
// one of the fixture's own steps, which share ctx's limit, and it's never
// retried.
func pull(ctx context.Context, image, dir string, env []string, w io.Writer) error {
	// decision: an image docker already has isn't pulled again, as docker
	// run pulls only a missing one, so a machine without the registry still
	// runs the image it has.
	if code, err := execute(ctx, []string{"docker", "image", "inspect", image}, dir, env, io.Discard, false); err == nil && code == 0 {
		return nil
	}
	shown := "docker pull " + image
	_, _ = fmt.Fprintf(w, "[dt] $ %s\n", shown)
	code, err := execute(ctx, []string{"docker", "pull", image}, dir, env, w, false)
	switch {
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("`%s` exited %d", shown, code)
	}
	return nil
}

// removeContainer removes a container whose docker client was killed. It has
// a minute of its own, since the test's time, or the run, is over.
func removeContainer(name, dir string, env []string, w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	argv := []string{"docker", "rm", "-f", name}
	_, _ = fmt.Fprintf(w, "[dt] $ %s\n", strings.Join(argv, " "))
	_, _ = execute(ctx, argv, dir, env, w, false)
}

// renderTemplates fills the placeholders in each file glob matches in the
// repository at root, and writes it to dir under its own name.
func renderTemplates(root, glob, dir string, vals map[string]string, lists map[string][]string) error {
	files, err := filepath.Glob(filepath.Join(root, glob))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		// decision: values go in unescaped, so a value can't hold what the
		// file's format treats specially, such as & or < in XML. Escape per
		// format once a value can.
		s, err := replace(string(data), vals, lists, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		// decision: a template's name loses its .template, as WPTS reads
		// its files by their own names.
		name := strings.TrimSuffix(filepath.Base(f), ".template")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			return err
		}
	}
	return nil
}
