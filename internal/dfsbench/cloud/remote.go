package cloud

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/config"
	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// Cloud orchestration primitives: provision one disposable SCW VM, drive the
// managed run on it detached (survives ssh drops via a DONE sentinel), gather
// results, tear it down. `setup` and `teardown` own the VM lifecycle; the run
// package's `run --remote` drives it through these helpers.

const remoteUser = "root"

// SSHCfg mirrors the old script's ssh options (no host-key prompts, keepalive)
// so a dropped session doesn't abort a long run.
func SSHCfg() exec.SSHConfig {
	return exec.SSHConfig{
		KeyPath: os.Getenv("SCW_SSH_KEY"),
		ExtraOpts: []string{
			"StrictHostKeyChecking=no",
			"UserKnownHostsFile=/dev/null",
			"ConnectTimeout=10",
			"ServerAliveInterval=30",
			"ServerAliveCountMax=6",
		},
	}
}

// BuildDriver wraps the remote run in a detached driver: source creds, run,
// always drop DONE so polling stops and partial results/log are gathered.
func BuildDriver(runCmd string) string {
	return strings.Join([]string{
		"#!/bin/sh",
		"rm -f /root/DONE",
		"set -a; [ -f /root/bench.env ] && . /root/bench.env; set +a",
		"rm -rf /root/bench-results && mkdir -p /root/bench-results", // fresh: a failed backend must not re-pull a prior run's cells
		runCmd + " > /root/run.log 2>&1",
		"echo $? > /root/EXIT",
		"touch /root/DONE",
		"",
	}, "\n")
}

// PollDone waits for /root/DONE, tailing the last log line each tick. Caps at
// ~2h (240 × 30s) — a long-run backstop, not an expected wait.
func PollDone(ctx context.Context, ex exec.Executor, vm VM) error {
	for i := 0; i < 240; i++ {
		if _, err := ex.Run(ctx, vm.IP, remoteUser, "test -f /root/DONE"); err == nil {
			return nil
		}
		if out, err := ex.Run(ctx, vm.IP, remoteUser, "tail -n1 /root/run.log 2>/dev/null"); err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				_, _ = fmt.Fprintf(exec.CmdOut, "  %s\n", s)
			}
		}
		time.Sleep(30 * time.Second)
	}
	return fmt.Errorf("timed out waiting for /root/DONE (partial results may exist on VM)")
}

// PushRemoteConfig writes the non-secret bucket/endpoint config and a 0600 creds
// env file to the VM (creds stay off argv and logs).
func PushRemoteConfig(ctx context.Context, ex exec.Executor, vm VM, cfg config.Config) error {
	bucket := orEnv(cfg.Bucket, "BENCH_BUCKET")
	endpoint := orEnv(cfg.Endpoint, "BENCH_ENDPOINT")
	yaml := fmt.Sprintf("bucket: %q\nendpoint: %q\n", bucket, endpoint)
	if err := ex.Push(ctx, MustTemp(yaml), vm.IP, remoteUser, "/root/dfsbench.yaml"); err != nil {
		return err
	}
	// Creds: 0600 env file, pushed (kept off argv and logs).
	env := ""
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			env += fmt.Sprintf("export %s=%q\n", k, v)
		}
	}
	envFile := mustTempMode(env, 0o600)
	if err := ex.Push(ctx, envFile, vm.IP, remoteUser, "/root/bench.env"); err != nil {
		return err
	}
	_, err := ex.Run(ctx, vm.IP, remoteUser, "chmod 600 /root/bench.env")
	return err
}

func orEnv(v, key string) string {
	if v != "" {
		return v
	}
	return os.Getenv(key)
}

// PullDir scp -r's a remote directory's contents into localDir.
func PullDir(ctx context.Context, vm VM, remoteDir, localDir string) error {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	cfg := SSHCfg()
	var args []string
	if cfg.KeyPath != "" {
		args = append(args, "-i", cfg.KeyPath)
	}
	for _, o := range cfg.ExtraOpts {
		args = append(args, "-o", o)
	}
	args = append(args, "-r", fmt.Sprintf("%s@%s:%s/.", remoteUser, vm.IP, remoteDir), localDir)
	c := osexec.CommandContext(ctx, "scp", args...)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("pull %s: %w\n%s", remoteDir, err, out)
	}
	return nil
}

// MustTemp writes s to a temp file and returns its path (caller-transient; the
// disposable-VM harness leaves cleanup to the OS temp reaper).
func MustTemp(s string) string { return mustTempMode(s, 0o644) }

func mustTempMode(s string, mode os.FileMode) string {
	f, err := os.CreateTemp("", "dfsbench-*")
	if err != nil {
		panic(err) // local temp file creation failing is not recoverable here
	}
	_, _ = f.WriteString(s)
	_ = f.Close()
	_ = os.Chmod(f.Name(), mode)
	return f.Name()
}
