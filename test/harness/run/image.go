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
)

// notStarted are docker run's own exit statuses: its container didn't start.
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
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name = "dt-" + notInName.ReplaceAllString(slug(t.cell.Name), "-") + "-" + hex.EncodeToString(b)
	docker = []string{"docker", "run", "--rm", "--name", name, "--network", network()}
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

// network is the network the fixture's server is on: the host's, unless dt
// runs in a dtc container on a bridge network, whose network the image's
// container then shares.
func network() string {
	if os.Getenv("DT_IN_CONTAINER") == "1" && os.Getenv("DT_HOSTNET") != "1" {
		return "container:" + os.Getenv("DT_CONTAINER_NAME")
	}
	return "host"
}

// removeContainer removes a container whose docker client was killed. It has
// a minute of its own, since the test's time, or the run, is over.
func removeContainer(name, dir string, env []string, w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	argv := []string{"docker", "rm", "-f", name}
	_, _ = fmt.Fprintf(w, "[dt] $ %s\n", strings.Join(argv, " "))
	_, _ = execute(ctx, argv, dir, env, w)
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
