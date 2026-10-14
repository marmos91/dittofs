package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeTools puts fakes first on PATH, so no test here can reach a real
// container, a real port or a real mount, whatever the code under test does:
//   - docker records each call in the returned file, prefixed with the
//     COMPOSE_PROFILES it got, if any, and exits with FAKE_DOCKER_STATUS, 1
//     unless a test sets it, failing any call that contains FAKE_DOCKER_FAIL;
//   - lsof, ss and nc report every port free;
//   - mount with no arguments prints the file FAKE_MOUNTS, an empty table
//     unless a test sets it; any other mount, and umount, mount_smbfs,
//     diskutil and sudo, record the call and exit with FAKE_MOUNT_STATUS, 0
//     unless a test sets it.
//
// HOME is a temporary directory too, so nothing under the real one is touched.
func fakeTools(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\n"+
		"echo \"${COMPOSE_PROFILES:+COMPOSE_PROFILES=$COMPOSE_PROFILES }docker $*\" >> '"+calls+"'\n"+
		"if [ -n \"$FAKE_DOCKER_FAIL\" ]; then case \"$*\" in *\"$FAKE_DOCKER_FAIL\"*) exit 1 ;; esac; fi\n"+
		"exit \"${FAKE_DOCKER_STATUS:-1}\"\n")
	writeFile(t, filepath.Join(bin, "lsof"), "#!/bin/sh\nexit 1\n")
	writeFile(t, filepath.Join(bin, "ss"), "#!/bin/sh\nexit 0\n")
	writeFile(t, filepath.Join(bin, "nc"), "#!/bin/sh\nexit 1\n")
	for _, name := range []string{"mount", "umount", "mount_smbfs", "diskutil", "sudo"} {
		writeFile(t, filepath.Join(bin, name), "#!/bin/sh\n"+
			"if [ \"$(basename \"$0\")\" = mount ] && [ $# -eq 0 ]; then exec cat \"${FAKE_MOUNTS:-/dev/null}\"; fi\n"+
			"echo \"$(basename \"$0\") $*\" >> '"+calls+"'\n"+
			"exit \"${FAKE_MOUNT_STATUS:-0}\"\n")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FAKE_DOCKER_STATUS", "")
	t.Setenv("FAKE_DOCKER_FAIL", "")
	t.Setenv("FAKE_MOUNTS", "")
	t.Setenv("FAKE_MOUNT_STATUS", "")
	t.Setenv("COMPOSE_PROFILES", "")
	return calls
}

// takeCalls returns the docker calls recorded so far, and forgets them.
func takeCalls(t *testing.T, calls string) string {
	t.Helper()
	data, _ := os.ReadFile(calls)
	_ = os.Remove(calls)
	return string(data)
}

// envCopy copies env.sh into a temporary checkout, so its state directory is
// temporary too, and returns the copy and that checkout.
func envCopy(t *testing.T) (script, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the environment commands are bash")
	}
	body, err := os.ReadFile("../../test/harness/env.sh")
	if err != nil {
		t.Fatal(err)
	}
	root = t.TempDir()
	script = filepath.Join(root, "test", "harness", "env.sh")
	writeFile(t, script, string(body))
	return script, root
}

// envRun runs the env.sh copy and returns its output and exit status.
func envRun(t *testing.T, script string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command("bash", append([]string{script}, args...)...).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return string(out), 0
}

// dt hands each environment command to test/harness/env.sh with its arguments
// and input, and exits with its status. A stub stands in for env.sh, in a
// checkout of its own, so the real script never runs here.
func TestEnvDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the environment commands are bash")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"),
		"version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\nsuites:\n  a: { cmd: \"true\" }\n")
	writeFile(t, filepath.Join(root, "test", "harness", "env.sh"),
		"printf '%s\\n' \"$@\" > \"$(dirname \"$0\")/argv\"\ncat > \"$(dirname \"$0\")/stdin\"\nexit 5\n")
	t.Chdir(root)
	for _, c := range envCommands {
		var out bytes.Buffer
		cmd := newRoot(&out)
		cmd.SetErr(&out)
		cmd.SetIn(strings.NewReader("input\n"))
		cmd.SetArgs([]string{c.name, "up", "two words", "--flag"})
		err := cmd.Execute()
		var status exitStatus
		if !errors.As(err, &status) || status != 5 {
			t.Errorf("dt %s: %v, want the script's exit status 5\n%s", c.name, err, out.String())
		}
		argv, _ := os.ReadFile(filepath.Join(root, "test", "harness", "argv"))
		if want := c.name + "\nup\ntwo words\n--flag\n"; string(argv) != want {
			t.Errorf("dt %s: the script got %q, want %q", c.name, argv, want)
		}
		if stdin, _ := os.ReadFile(filepath.Join(root, "test", "harness", "stdin")); string(stdin) != "input\n" {
			t.Errorf("dt %s: the script read %q", c.name, stdin)
		}
	}
}

// env.sh refuses what it doesn't know before it touches Docker. The fake
// docker records any call, so a check that let something through fails here,
// and still never reaches a real container.
func TestEnvRefusesBeforeDocker(t *testing.T) {
	script, _ := envCopy(t)
	calls := fakeTools(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"services", "up", "bogus"}, "unknown service: bogus"},
		{[]string{"services", "down", "bogus"}, "unknown service: bogus"},
		{[]string{"services", "bogus"}, "usage: dt services"},
		{[]string{"stack", "bogus"}, "usage: dt stack"},
		{[]string{"stack", "down", "-x"}, "usage: dt stack"},
		{[]string{"stack", "up", "--memroy"}, "usage: dt stack"},
		{[]string{"stack", "up", "--memory", "extra"}, "usage: dt stack"},
		{[]string{"stack", "status", "junk"}, "usage: dt stack"},
		{[]string{"cleanup", "--bogus"}, "usage: dt cleanup"},
		{[]string{"bogus"}, "usage: env.sh"},
	}
	for _, c := range cases {
		args := strings.Join(c.args, " ")
		if out, status := envRun(t, script, c.args...); status != 2 || !strings.Contains(out, c.want) {
			t.Errorf("env.sh %s: exit status %d, want 2 and %q\n%s", args, status, c.want, out)
		}
		if got := takeCalls(t, calls); got != "" {
			t.Errorf("env.sh %s called docker:\n%s", args, got)
		}
	}
}

// A service starts under its own name, published on 127.0.0.1 only, and
// postgres is ready only once it answers over TCP.
func TestServicesUpOnLoopback(t *testing.T) {
	script, _ := envCopy(t)
	calls := fakeTools(t)
	t.Setenv("FAKE_DOCKER_STATUS", "0")
	if out, status := envRun(t, script, "services", "up", "postgres"); status != 0 {
		t.Fatalf("services up postgres: exit status %d\n%s", status, out)
	}
	got := takeCalls(t, calls)
	for _, want := range []string{
		"docker run -d --name dittofs-harness-pg-it -p 127.0.0.1:15432:5432 ",
		"docker exec dittofs-harness-pg-it pg_isready -h 127.0.0.1 -U postgres",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q among the docker calls:\n%s", want, got)
		}
	}
}

// kmip reads its pins from start-pykmip.sh, quoted or not, and writes its
// environment file whole or not at all: a failed provisioning leaves none.
func TestKMIP(t *testing.T) {
	script, _ := envCopy(t)
	calls := fakeTools(t)
	t.Setenv("FAKE_DOCKER_STATUS", "0")
	repo := t.TempDir()
	gen, err := os.ReadFile("../../test/kmip/gen-certs.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "test", "kmip", "gen-certs.sh"), string(gen))
	writeFile(t, filepath.Join(repo, "test", "kmip", "start-pykmip.sh"),
		"IMAGE=\"python:3.11-slim\"\nPYKMIP_VERSION='0.10.0'  # pinned\n")
	t.Setenv("DITTOFS_REPO", repo)
	envFile := filepath.Join(filepath.Dir(script), "state", "kmip.env")

	if out, status := envRun(t, script, "services", "up", "kmip"); status != 0 {
		t.Fatalf("services up kmip: exit status %d\n%s", status, out)
	}
	got := takeCalls(t, calls)
	if !strings.Contains(got, "-p 127.0.0.1:5696:5696 ") || !strings.Contains(got, "docker.io/library/python:3.11-slim sh -c pip install --no-cache-dir pykmip==0.10.0 ") {
		t.Errorf("the run doesn't use the pins on loopback:\n%s", got)
	}
	if env, _ := os.ReadFile(envFile); !strings.HasPrefix(string(env), "DITTOFS_TEST_KMIP=1\n") {
		t.Errorf("kmip.env: %q", env)
	}

	t.Setenv("FAKE_DOCKER_FAIL", "provision.py")
	if out, status := envRun(t, script, "services", "up", "kmip"); status == 0 {
		t.Errorf("a failed provisioning passed\n%s", out)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Errorf("a failed provisioning left kmip.env: %v", err)
	}
}

// dt stack down -v passes -v on, so the stack's volumes go too.
func TestStackDownVolumes(t *testing.T) {
	script, _ := envCopy(t)
	calls := fakeTools(t)
	t.Setenv("FAKE_DOCKER_STATUS", "0")
	if out, status := envRun(t, script, "stack", "down", "-v"); status != 0 {
		t.Fatalf("stack down -v: exit status %d\n%s", status, out)
	}
	if got := takeCalls(t, calls); !strings.Contains(got, "docker compose --profile s3-backend down -v\n") {
		t.Errorf("no down -v among the docker calls:\n%s", got)
	}
}

// dt stack up --memory starts no profile, even with COMPOSE_PROFILES exported.
// The fake docker fails the up, so the run stops there.
func TestStackUpMemory(t *testing.T) {
	script, _ := envCopy(t)
	calls := fakeTools(t)
	t.Setenv("FAKE_DOCKER_STATUS", "0")
	t.Setenv("FAKE_DOCKER_FAIL", "up -d")
	t.Setenv("COMPOSE_PROFILES", "s3-backend")
	out, _ := envRun(t, script, "stack", "up", "--memory")
	got := takeCalls(t, calls)
	if !strings.Contains(got, "\ndocker compose up -d --build\n") {
		t.Errorf("the up isn't free of profiles:\n%s\n%s", got, out)
	}
}

// dt stack mount and umount read the whole mount table, so a long one still
// shows what is mounted, and a failed unmount fails umount.
func TestStackMounts(t *testing.T) {
	script, _ := envCopy(t)
	calls := fakeTools(t)
	t.Setenv("FAKE_DOCKER_STATUS", "0")
	t.Setenv("DT_IN_CONTAINER", "") // inside dtc, mount and umount only point to the host
	nfs := filepath.Join(os.Getenv("HOME"), "mnt", "dittofs-nfs")
	smb := filepath.Join(os.Getenv("HOME"), "mnt", "dittofs-smb")
	// Both mounts first, then far more than a pipe holds: a grep -q that
	// stops at the first line kills the rest of the listing with SIGPIPE.
	mounts := filepath.Join(t.TempDir(), "mounts")
	writeFile(t, mounts, "x on "+nfs+" (nfs)\nx on "+smb+" (smbfs)\n"+
		strings.Repeat("filler on /filler (apfs)\n", 50000))
	t.Setenv("FAKE_MOUNTS", mounts)

	out, status := envRun(t, script, "stack", "mount")
	if status != 0 || !strings.Contains(out, "NFS already mounted") || !strings.Contains(out, "SMB already mounted") {
		t.Errorf("stack mount: exit status %d, want 0 and both already mounted\n%s", status, out)
	}
	takeCalls(t, calls)

	if out, status := envRun(t, script, "stack", "umount"); status != 0 {
		t.Errorf("stack umount: exit status %d\n%s", status, out)
	}
	got := takeCalls(t, calls)
	for _, m := range []string{nfs, smb} {
		if !strings.Contains(got, "umount "+m+"\n") {
			t.Errorf("no umount of %s among the calls:\n%s", m, got)
		}
	}

	t.Setenv("FAKE_MOUNT_STATUS", "1")
	if out, status := envRun(t, script, "stack", "umount"); status == 0 || !strings.Contains(out, "could not unmount "+nfs) {
		t.Errorf("stack umount with every unmount failing: exit status %d\n%s", status, out)
	}
}
