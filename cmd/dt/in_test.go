package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// inCheckout is a checkout whose suite lin has the need linux, and whose dtc
// records the command line it was given and exits with $DTC_EXIT, 2 by
// default, so a run sent to the container is told from one that ran here.
func inCheckout(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake dtc is a shell script")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\nsuites:\n  lin: { cmd: \"true\", needs: [linux] }\n")
	writeFile(t, filepath.Join(root, "test", "harness", "bin", "dtc"), "#!/bin/sh\necho \"$@\" > \"$(dirname \"$0\")/args\"\nexit \"${DTC_EXIT:-2}\"\n")
	t.Chdir(root)
	return root
}

// sent returns the command line the fake dtc got, or "" when it didn't run,
// and clears it for the next run.
func sent(root string) string {
	file := filepath.Join(root, "test", "harness", "bin", "args")
	b, _ := os.ReadFile(file)
	_ = os.Remove(file)
	return strings.TrimSpace(string(b))
}

// --in container runs the same command in dtc, with --in host, and dtc's exit
// status is the run's, any but 1 to 3 as 2. Inside dtc it runs there; --in
// takes only host, container and auto, and dtc takes no results outside the
// checkout.
func TestRunInContainer(t *testing.T) {
	root := inCheckout(t)
	results := filepath.Join(root, "results")
	var status exitStatus
	if _, err := dt(t, "run", "lin", "--in", "container", "--results", results); !errors.As(err, &status) || status != 2 {
		t.Errorf("dt run lin --in container: %v, want dtc's exit status 2", err)
	}
	if got, want := sent(root), "run lin --results="+results+" --in=host"; got != want {
		t.Errorf("dtc got %q, want %q", got, want)
	}
	for code, want := range map[string]exitStatus{"1": 1, "125": 2} {
		t.Setenv("DTC_EXIT", code)
		if _, err := dt(t, "run", "lin", "--in", "container", "--results", results); !errors.As(err, &status) || status != want {
			t.Errorf("dtc exits %s: %v, want exit status %d", code, err, want)
		}
		sent(root)
	}
	if _, err := dt(t, "run", "lin", "--in", "vm"); err == nil || errors.As(err, &status) || sent(root) != "" {
		t.Errorf("dt run lin --in vm: %v, want a usage error", err)
	}
	if _, err := dt(t, "run", "lin", "--in", "container", "--results", t.TempDir()); err == nil || errors.As(err, &status) || sent(root) != "" {
		t.Errorf("dt run lin --in container --results outside: %v, want a usage error", err)
	}
	t.Setenv("DT_IN_CONTAINER", "1")
	_, _ = dt(t, "run", "lin", "--in", "container", "--results", results)
	if got := sent(root); got != "" {
		t.Errorf("inside dtc, dtc ran again: %q", got)
	}
}

// --in auto, the default, sends the run to dtc when a cell needs what this
// machine lacks and the container gives; with --ci it stays here, and so does
// a run whose needs are met or a machine without Docker, which dtc runs in.
func TestRunInAuto(t *testing.T) {
	root := inCheckout(t)
	results := filepath.Join(root, "results")
	linux, docker := false, true
	was := need
	need = func(_, n string) error {
		switch {
		case n == "linux" && !linux:
			return errors.New("not Linux")
		case n == "docker" && !docker:
			return errors.New("no Docker")
		}
		return nil
	}
	t.Cleanup(func() { need = was })
	for _, c := range []struct {
		linux, docker bool
		flags         []string
		sent          bool
	}{
		{false, true, nil, true},
		{false, true, []string{"--ci"}, false},
		{true, true, nil, false},
		{false, false, nil, false},
	} {
		linux, docker = c.linux, c.docker
		_, _ = dt(t, append([]string{"run", "lin", "--results", results}, c.flags...)...)
		if got := sent(root) != ""; got != c.sent {
			t.Errorf("Linux here %v, Docker here %v, flags %v: sent to dtc %v, want %v", c.linux, c.docker, c.flags, got, c.sent)
		}
	}
}
