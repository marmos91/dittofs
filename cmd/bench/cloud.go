package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Cloud orchestration: provision one disposable SCW VM, drive the managed run on
// it detached (survives ssh drops via a DONE sentinel), gather results, tear it
// down. `setup` and `teardown` own the VM lifecycle; `run --remote` drives it.

const remoteUser = "root"

// sshCfg mirrors the old script's ssh options (no host-key prompts, keepalive)
// so a dropped session doesn't abort a long run.
func sshCfg() SSHConfig {
	return SSHConfig{
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

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Provision a disposable SCW bench VM and push the dfsbench binary",
		Long: `setup creates one Scaleway VM (SCW_* env overrides type/zone/image),
waits for SSH, cross-builds dfsbench for linux/amd64, pushes it, and records the
VM handle in .bench-vm.json so 'run --remote' and 'teardown' reattach.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd.Context()) },
	}
}

func runSetup(ctx context.Context) error {
	vm, err := provisionVM(ctx, defaultVMSpec())
	// Persist whatever we got before anything else can fail, so teardown always
	// has a handle to clean up (even a half-provisioned VM).
	if vm.ServerID != "" {
		_ = saveVM(vm)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmdOut, "provisioned %s (%s)\n", vm.ServerID, vm.IP)

	ex := NewSSHExecutor(sshCfg())
	if err := waitSSH(ctx, ex, vm); err != nil {
		return err
	}
	// Push dfsbench plus the DittoFS server + client (the dittofs-s3 subject
	// runs `dfs`/`dfsctl` on the VM). All are pure-Go / CGO-free cross-builds.
	for _, b := range []struct{ pkg, name, dst string }{
		{"./cmd/bench", "dfsbench", "/root/dfsbench"},
		{"./cmd/dfs", "dfs", "/usr/local/bin/dfs"},
		{"./cmd/dfsctl", "dfsctl", "/usr/local/bin/dfsctl"},
	} {
		bin, err := crossBuild(ctx, b.pkg, b.name)
		if err != nil {
			return err
		}
		err = ex.Push(ctx, bin, vm.IP, remoteUser, b.dst)
		_ = os.RemoveAll(filepath.Dir(bin))
		if err != nil {
			return err
		}
		if _, err := ex.Run(ctx, vm.IP, remoteUser, "chmod +x "+b.dst); err != nil {
			return err
		}
	}
	if err := installPrereqs(ctx, ex, vm); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmdOut, "ready: dfsbench run --remote --systems ...  (then dfsbench teardown)\n")
	return nil
}

// installPrereqs installs the shared load generator (fio — needed by every
// cell), the netcat used by waitPort, and every backend's server/client
// packages in one apt transaction. Front-loading here makes per-backend Setup a
// fast `command -v` no-op and guarantees fio exists before any run. (juicefs is
// not in apt — its recipe curl-installs it.)
func installPrereqs(ctx context.Context, ex Executor, vm VM) error {
	const pkgs = "fio netcat-openbsd curl fuse3 " + // shared
		"nfs-kernel-server samba cifs-utils " + // re-export layer
		"s3fs s3ql rclone" // FUSE competitors in apt
	_, _ = fmt.Fprintln(cmdOut, "installing prerequisites (fio + backend packages)…")
	_, err := ex.Run(ctx, vm.IP, remoteUser,
		"DEBIAN_FRONTEND=noninteractive apt-get update && "+
			"DEBIAN_FRONTEND=noninteractive apt-get install -y "+pkgs)
	return err
}

func newTeardownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "teardown",
		Short: "Terminate the bench VM recorded in .bench-vm.json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			vm, err := loadVM()
			if err != nil {
				return err
			}
			if err := terminateVM(cmd.Context(), vm); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmdOut, "terminated %s\n", vm.ServerID)
			return clearVMState()
		},
	}
}

// waitSSH blocks until the VM accepts an ssh command (60 × 5s).
func waitSSH(ctx context.Context, ex Executor, vm VM) error {
	for i := 0; i < 60; i++ {
		if _, err := ex.Run(ctx, vm.IP, remoteUser, "true"); err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("ssh to %s never came up", vm.IP)
}

// crossBuild builds a static linux/amd64 binary from pkg and returns its path.
func crossBuild(ctx context.Context, pkg, name string) (string, error) {
	dir, err := os.MkdirTemp("", "dfsbench-build-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, name)
	c := exec.CommandContext(ctx, "go", "build", "-o", bin, pkg)
	c.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-build %s: %w\n%s", pkg, err, out)
	}
	return bin, nil
}

// runRemote drives the managed run on the VM, detached. Called from runBench
// when --remote is set; the run flags are forwarded to the VM's dfsbench.
func runRemote(ctx context.Context, f *runFlags) error {
	vm, err := loadVM()
	if err != nil {
		return err
	}
	ex := NewSSHExecutor(sshCfg())

	// Non-secret bucket/endpoint go in a config on the VM; creds go in a 0600
	// env file (never on argv/logs). Both are pushed, not passed as flags.
	cfg, err := loadConfig(f.config)
	if err != nil {
		return err
	}
	if err := pushRemoteConfig(ctx, ex, vm, cfg); err != nil {
		return err
	}

	driver := buildDriver(remoteRunArgs(f))
	if err := ex.Push(ctx, mustTemp(driver), vm.IP, remoteUser, "/root/driver.sh"); err != nil {
		return err
	}
	// Launch detached: the ssh session returns immediately; the run keeps going
	// and drops /root/DONE when finished (survives our ssh dropping).
	if _, err := ex.Run(ctx, vm.IP, remoteUser, "nohup sh /root/driver.sh >/dev/null 2>&1 &"); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmdOut, "launched detached on VM; polling for completion…")
	if err := pollDone(ctx, ex, vm); err != nil {
		return err
	}
	// Gather results + log, then render locally from disk.
	if err := pullDir(ctx, vm, "/root/bench-results", f.results); err != nil {
		return err
	}
	_ = ex.Pull(ctx, vm.IP, remoteUser, "/root/run.log", f.results+"/run.log")
	all, err := loadResults(f.results)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmdOut)
	_, _ = fmt.Fprint(cmdOut, renderTable(all))
	return nil
}

// remoteRunArgs forwards the local run selection to the VM's dfsbench.
func remoteRunArgs(f *runFlags) string {
	args := []string{"run", "--config", "/root/dfsbench.yaml", "--results", "/root/bench-results"}
	if len(f.systems) > 0 {
		args = append(args, "--systems", strings.Join(f.systems, ","))
	}
	if len(f.workloads) > 0 {
		args = append(args, "--workloads", strings.Join(f.workloads, ","))
	}
	if len(f.sizes) > 0 {
		args = append(args, "--sizes", strings.Join(f.sizes, ","))
	}
	if !f.evictCache {
		args = append(args, "--evict-cache=false")
	}
	if f.resume {
		args = append(args, "--resume")
	}
	return "/root/dfsbench " + strings.Join(args, " ")
}

// buildDriver wraps the remote run in a detached driver: source creds, run,
// always drop DONE so polling stops and partial results/log are gathered.
func buildDriver(runCmd string) string {
	return strings.Join([]string{
		"#!/bin/sh",
		"rm -f /root/DONE",
		"set -a; [ -f /root/bench.env ] && . /root/bench.env; set +a",
		"mkdir -p /root/bench-results",
		runCmd + " > /root/run.log 2>&1",
		"echo $? > /root/EXIT",
		"touch /root/DONE",
		"",
	}, "\n")
}

// pollDone waits for /root/DONE, tailing the last log line each tick. Caps at
// ~2h (240 × 30s) — a long-run backstop, not an expected wait.
func pollDone(ctx context.Context, ex Executor, vm VM) error {
	for i := 0; i < 240; i++ {
		if _, err := ex.Run(ctx, vm.IP, remoteUser, "test -f /root/DONE"); err == nil {
			return nil
		}
		if out, err := ex.Run(ctx, vm.IP, remoteUser, "tail -n1 /root/run.log 2>/dev/null"); err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				_, _ = fmt.Fprintf(cmdOut, "  %s\n", s)
			}
		}
		time.Sleep(30 * time.Second)
	}
	return fmt.Errorf("timed out waiting for /root/DONE (partial results may exist on VM)")
}

func pushRemoteConfig(ctx context.Context, ex Executor, vm VM, cfg Config) error {
	bucket := orEnv(cfg.Bucket, "BENCH_BUCKET")
	endpoint := orEnv(cfg.Endpoint, "BENCH_ENDPOINT")
	yaml := fmt.Sprintf("bucket: %q\nendpoint: %q\n", bucket, endpoint)
	if err := ex.Push(ctx, mustTemp(yaml), vm.IP, remoteUser, "/root/dfsbench.yaml"); err != nil {
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

// pullDir scp -r's a remote directory's contents into localDir.
func pullDir(ctx context.Context, vm VM, remoteDir, localDir string) error {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	cfg := sshCfg()
	var args []string
	if cfg.KeyPath != "" {
		args = append(args, "-i", cfg.KeyPath)
	}
	for _, o := range cfg.ExtraOpts {
		args = append(args, "-o", o)
	}
	args = append(args, "-r", fmt.Sprintf("%s@%s:%s/.", remoteUser, vm.IP, remoteDir), localDir)
	c := exec.CommandContext(ctx, "scp", args...)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("pull %s: %w\n%s", remoteDir, err, out)
	}
	return nil
}

// mustTemp writes s to a temp file and returns its path (caller-transient; the
// disposable-VM harness leaves cleanup to the OS temp reaper).
func mustTemp(s string) string { return mustTempMode(s, 0o644) }

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
