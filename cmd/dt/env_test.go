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
// container or a real port, whatever the code under test does:
//   - docker records each call in the returned file and exits with
//     FAKE_DOCKER_STATUS, 1 unless a test sets it, failing any call that
//     contains FAKE_DOCKER_FAIL;
//   - lsof, ss and nc report every port free.
//
// HOME is a temporary directory too, so nothing under the real one is touched.
func fakeTools(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"docker $*\" >> '"+calls+"'\n"+
		"if [ -n \"$FAKE_DOCKER_FAIL\" ]; then case \"$*\" in *\"$FAKE_DOCKER_FAIL\"*) exit 1 ;; esac; fi\n"+
		"exit \"${FAKE_DOCKER_STATUS:-1}\"\n")
	writeFile(t, filepath.Join(bin, "lsof"), "#!/bin/sh\nexit 1\n")
	writeFile(t, filepath.Join(bin, "ss"), "#!/bin/sh\nexit 0\n")
	writeFile(t, filepath.Join(bin, "nc"), "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FAKE_DOCKER_STATUS", "")
	t.Setenv("FAKE_DOCKER_FAIL", "")
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
		{[]string{"hooks", "bogus"}, "usage: dt hooks"},
		{[]string{"hooks", "install", "--bogus"}, "usage: dt hooks"},
		{[]string{"hooks", "status", "junk"}, "usage: dt hooks"},
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

// gitless clears the variables git sets for a hook, which a hook running these
// tests exports, so git works on the test's own repository.
func gitless(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
}

// tempRepo makes a throwaway git repository.
func tempRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return repo
}

// dt hooks install runs the repository's own hooks, --container the harness's,
// which run them in dtc, and uninstall goes back to the repository's own. The
// paths are relative, so each worktree runs its own. GIT_DIR pins every git
// command to a throwaway repository, so no real one's hooks can change.
func TestHooksInstall(t *testing.T) {
	script, _ := envCopy(t)
	gitless(t)
	calls := fakeTools(t)
	repo := tempRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(repo, ".git"))
	t.Setenv("DITTOFS_REPO", repo)
	hooksPath := func() string {
		out, _ := exec.Command("git", "config", "--get", "core.hooksPath").Output()
		return strings.TrimSpace(string(out))
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"install", "--container"}, "test/harness/githooks"},
		{[]string{"install"}, ".githooks"},
		{[]string{"install", "--container"}, "test/harness/githooks"},
		{[]string{"uninstall"}, ".githooks"},
	} {
		if out, status := envRun(t, script, append([]string{"hooks"}, c.args...)...); status != 0 {
			t.Fatalf("hooks %s: exit status %d\n%s", strings.Join(c.args, " "), status, out)
		}
		if got := hooksPath(); got != c.want {
			t.Errorf("after hooks %s: core.hooksPath %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
		if out, _ := envRun(t, script, "hooks", "status"); !strings.Contains(out, "core.hooksPath = "+c.want) {
			t.Errorf("status after hooks %s:\n%s", strings.Join(c.args, " "), out)
		}
	}
	if got := takeCalls(t, calls); got != "" {
		t.Errorf("dt hooks called docker:\n%s", got)
	}
}

// dtcDocker puts first on PATH a docker that passes dtc's checks and stands in
// for the container: it records each call's command in dir/calls and, for run,
// its arguments, one per line, in dir/argv and its input in dir/stdin, and
// exits 4. dtc goes as far as docker run, and no further.
func dtcDocker(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "docker"), "#!/bin/sh\necho \"docker $1\" >> '"+dir+"/calls'\ncase \"$1\" in\n"+
		"context) echo unix:///var/run/docker.sock ;;\n"+
		"run) printf '%s\\n' \"$@\" > '"+dir+"/argv'; cat > '"+dir+"/stdin'; exit 4 ;;\n"+
		"esac\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "")
	return dir
}

// A harness hook sends the repository's own hook of the same name to dtc, with
// its arguments and input and the repository, and returns the container's exit
// status; in the container it runs that hook with its arguments and input, and
// returns its exit status.
func TestHarnessHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hooks are bash")
	}
	gitless(t)
	fakeTools(t)
	docker := dtcDocker(t)
	hook, err := filepath.Abs("../../test/harness/githooks/pre-push")
	if err != nil {
		t.Fatal(err)
	}
	for _, inContainer := range []string{"", "1"} {
		repo := tempRepo(t)
		writeFile(t, filepath.Join(repo, ".githooks", "pre-push"), "#!/bin/sh\n{ echo \"$@\"; cat; } > ran\nexit 3\n")
		t.Setenv("DT_IN_CONTAINER", inContainer)
		cmd := exec.Command(hook, "origin", "url")
		cmd.Dir, cmd.Stdin = repo, strings.NewReader("refs\n")
		err := cmd.Run()
		var exit *exec.ExitError
		ran, _ := os.ReadFile(filepath.Join(repo, "ran"))
		calls := takeCalls(t, filepath.Join(docker, "calls"))
		argv := takeCalls(t, filepath.Join(docker, "argv"))
		stdin := takeCalls(t, filepath.Join(docker, "stdin"))
		if inContainer == "" {
			// On this machine: docker run gets the hook, its arguments and its
			// input, and the repository as git names it; the repository's hook
			// doesn't run here.
			top, _ := filepath.EvalSymlinks(repo)
			if !errors.As(err, &exit) || exit.ExitCode() != 4 || len(ran) != 0 || stdin != "refs\n" ||
				!strings.HasSuffix(argv, "\nshell\n"+hook+"\norigin\nurl\n") || !strings.Contains(argv, "\nDITTOFS_REPO="+top+"\n") {
				t.Errorf("on this machine: %v, hook output %q, docker run input %q, arguments:\n%s\nwant docker run's exit status 4, input \"refs\\n\", and shell %s origin url for %s",
					err, ran, stdin, argv, hook, top)
			}
			continue
		}
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || string(ran) != "origin url\nrefs\n" || calls != "" {
			t.Errorf("in the container: %v, hook output %q, docker calls %q; want the repository's hook and exit status 3", err, ran, calls)
		}
	}
}

// dtc reaches docker run when it can't read the host's timezone: TZ unset, and
// /etc/localtime not a link, which a readlink that fails stands for.
func TestDtcWithoutTimezone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dtc is bash")
	}
	gitless(t)
	fakeTools(t)
	docker := dtcDocker(t)
	writeFile(t, filepath.Join(docker, "readlink"), "#!/bin/sh\nexit 1\n")
	t.Setenv("TZ", "")
	_ = os.Unsetenv("TZ")
	t.Setenv("DITTOFS_REPO", tempRepo(t))
	out, err := exec.Command("../../test/harness/bin/dtc", "list").CombinedOutput()
	var exit *exec.ExitError
	if argv := takeCalls(t, filepath.Join(docker, "argv")); !errors.As(err, &exit) || exit.ExitCode() != 4 || !strings.HasSuffix(argv, "\nlist\n") {
		t.Errorf("dtc list: %v, docker run arguments %q; want docker run's exit status 4\n%s", err, argv, out)
	}
}
