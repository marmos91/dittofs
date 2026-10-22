package run

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/report"
)

// A cell that needs podman, on a machine whose podman is older than the
// scenarios need, has podman's image and its cache volume ready before its
// first test, and runs each command in that image as the scenarios' README
// runs setup.sh: privileged, as the image's user podman, with podman's own
// configuration, the checkout at /src/dittofs, the two volumes, and the
// suite's variables named. Where the machine's podman is new enough, the
// command runs on the host, and docker isn't called.
func TestPodmanImage(t *testing.T) {
	root, r := repo(t, `suites:
  sys: { cmd: { sh: 'echo ran "$(basename {test})" $V' }, tests: "t/*.sh", needs: [podman], env: { V: v1 } }
`, map[string]string{"t/a.sh": ""})
	log := fakeDocker(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(log), "podman"), []byte("#!/bin/sh\necho \"$FAKE_PODMAN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_PODMAN", "5.8.0")
	rep, out := runTarget(t, root, r, "sys", false)
	if rep.Outcomes[0].Result != report.Passed || !strings.Contains(out, "ran a.sh v1") {
		t.Fatalf("on the host: %+v\n%s", rep.Outcomes[0], out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatalf("docker was called with the host's podman new enough:\n%s", readFile(t, log))
	}

	t.Setenv("FAKE_PODMAN", "4.9.3")
	rep, out = runTarget(t, root, r, "sys", false)
	if rep.Outcomes[0].Result != report.Passed || !strings.Contains(out, "ran a.sh v1") || !strings.Contains(out, "env V=v1") {
		t.Fatalf("in podman's image: %+v\n%s", rep.Outcomes[0], out)
	}
	got := calls(t, log)
	name := regexp.MustCompile(`^dt-sys-[0-9a-f]{8}$`)
	for _, call := range got {
		for i, a := range call {
			call[i] = name.ReplaceAllString(strings.ReplaceAll(a, root, "ROOT"), "NAME")
		}
	}
	want := [][]string{
		{"info"},
		{"image", "inspect", podmanImage},
		{"run", "--rm", "-v", "dittofs-scenarios-cache:/cache", podmanImage, "chown", "podman:podman", "/cache"},
		{"run", "--rm", "--name", "NAME", "--privileged", "--user", "podman", "-e", "CONTAINERS_CONF=/etc/containers/containers.conf",
			"-v", "ROOT:/src/dittofs", "-w", "/src/dittofs",
			"-v", "dittofs-podman-store:/home/podman/.local/share/containers", "-v", "dittofs-scenarios-cache:/var/tmp/dittofs-scenarios",
			"-e", "V", podmanImage, "sh", "-c", `echo ran "$(basename 't/a.sh')" $V`},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("docker calls\n%q\nwant\n%q", got, want)
	}
}

// A command in podman's image that exits as docker run does when its
// container didn't start, or as podman does for its own errors, ran nothing:
// the cell is not run, not failed.
func TestPodmanImageNotStarted(t *testing.T) {
	root, r := repo(t, `suites:
  sys: { cmd: "sh {test}", tests: "t/*.sh", needs: [podman] }
`, map[string]string{"t/a.sh": "exit 125\n"})
	log := fakeDocker(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(log), "podman"), []byte("#!/bin/sh\necho 4.9.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, out := runTarget(t, root, r, "sys", false)
	if o := rep.Outcomes[0]; o.Result != report.NotRun || !strings.Contains(o.Note, "its container didn't start") {
		t.Errorf("%+v\n%s", o, out)
	}
}
