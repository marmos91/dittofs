package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/dfsbench/cloud"
	"github.com/marmos91/dittofs/internal/dfsbench/config"
	"github.com/marmos91/dittofs/internal/dfsbench/exec"
	"github.com/marmos91/dittofs/internal/dfsbench/fio"
	"github.com/marmos91/dittofs/internal/dfsbench/report"
)

// runRemote drives the managed run on the VM, detached. Called from runBench
// when --remote is set; the run flags are forwarded to the VM's dfsbench.
func runRemote(ctx context.Context, f *runFlags) error {
	vm, err := cloud.LoadVM()
	if err != nil {
		return err
	}
	ex := exec.NewSSHExecutor(cloud.SSHCfg())

	// Non-secret bucket/endpoint go in a config on the VM; creds go in a 0600
	// env file (never on argv/logs). Both are pushed, not passed as flags.
	cfg, err := config.LoadConfig(f.config)
	if err != nil {
		return err
	}
	if err := cloud.PushRemoteConfig(ctx, ex, vm, cfg); err != nil {
		return err
	}

	driver := cloud.BuildDriver(remoteRunArgs(f))
	if err := ex.Push(ctx, cloud.MustTemp(driver), vm.IP, "root", "/root/driver.sh"); err != nil {
		return err
	}
	// Clear any prior sentinel/log synchronously before launching, so pollDone
	// can't race and return on a stale DONE left by an earlier run.
	if _, err := ex.Run(ctx, vm.IP, "root", "rm -f /root/DONE /root/run.log"); err != nil {
		return err
	}
	// Launch detached: the ssh session returns immediately; the run keeps going
	// and drops /root/DONE when finished (survives our ssh dropping).
	if _, err := ex.Run(ctx, vm.IP, "root", "nohup sh /root/driver.sh >/dev/null 2>&1 &"); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(exec.CmdOut, "launched detached on VM; polling for completion…")
	if err := cloud.PollDone(ctx, ex, vm); err != nil {
		return err
	}
	// Gather results + log, then render locally from disk.
	if err := cloud.PullDir(ctx, vm, "/root/bench-results", f.results); err != nil {
		return err
	}
	_ = ex.Pull(ctx, vm.IP, "root", "/root/run.log", f.results+"/run.log")
	all, err := fio.LoadResults(f.results)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(exec.CmdOut)
	_, _ = fmt.Fprint(exec.CmdOut, report.RenderTable(all))
	return nil
}

// remoteRunArgs forwards the local run selection to the VM's dfsbench.
func remoteRunArgs(f *runFlags) string {
	args := []string{"run", "--config", "/root/dfsbench.yaml", "--results", "/root/bench-results"}
	if len(f.systems) > 0 {
		args = append(args, "--systems", shQuote(strings.Join(f.systems, ",")))
	}
	if len(f.workloads) > 0 {
		args = append(args, "--workloads", shQuote(strings.Join(f.workloads, ",")))
	}
	if len(f.sizes) > 0 {
		args = append(args, "--sizes", shQuote(strings.Join(f.sizes, ",")))
	}
	if f.runtime > 0 {
		args = append(args, "--runtime", fmt.Sprintf("%d", f.runtime))
	}
	if f.threads > 0 {
		args = append(args, "--threads", fmt.Sprintf("%d", f.threads))
	}
	if !f.evictCache {
		args = append(args, "--evict-cache=false")
	}
	if f.resume {
		args = append(args, "--resume")
	}
	if f.skipBaseline {
		args = append(args, "--skip-baseline")
	}
	return "/root/dfsbench " + strings.Join(args, " ")
}

// shQuote single-quotes a value forwarded into the remote /bin/sh command, so a
// backend/workload name with shell metacharacters can't be reinterpreted.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
