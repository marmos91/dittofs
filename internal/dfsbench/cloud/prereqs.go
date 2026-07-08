package cloud

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// NewSetupCmd builds the `setup` subcommand: provision a disposable VM and push
// the dfsbench binary (plus dfs/dfsctl) to it.
func NewSetupCmd() *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Provision a disposable bench VM and push the dfsbench binary",
		Long: `setup provisions one disposable VM (default provider: scw; SCW_* env
overrides type/zone/image), waits for SSH, cross-builds dfsbench for the VM's
arch, pushes it, and records the VM handle in .bench-vm.json so 'run --remote'
and 'teardown' reattach.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd.Context(), provider) },
	}
	cmd.Flags().StringVar(&provider, "provider", "scw", "cloud provider for the bench VM (supported: scw)")
	return cmd
}

func runSetup(ctx context.Context, providerName string) error {
	p, err := newProvider(providerName)
	if err != nil {
		return err
	}
	vm, err := p.Provision(ctx)
	// Persist whatever we got before anything else can fail, so teardown always
	// has a handle to clean up (even a half-provisioned VM).
	if vm.ServerID != "" {
		_ = saveVM(vm)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(exec.CmdOut, "provisioned %s (%s)\n", vm.ServerID, vm.IP)

	ex := exec.NewSSHExecutor(SSHCfg())
	if err := waitSSH(ctx, ex, vm); err != nil {
		return err
	}
	// Build for the VM's real arch (detected over ssh), so this works whatever
	// instance type the provider hands back — amd64 or arm64.
	arch, err := detectArch(ctx, ex, vm)
	if err != nil {
		return err
	}
	// Push dfsbench plus the DittoFS server + client (the dittofs-s3 subject
	// runs `dfs`/`dfsctl` on the VM). All are pure-Go / CGO-free cross-builds.
	for _, b := range []struct{ pkg, name, dst string }{
		{"./cmd/bench", "dfsbench", "/root/dfsbench"},
		{"./cmd/dfs", "dfs", "/usr/local/bin/dfs"},
		{"./cmd/dfsctl", "dfsctl", "/usr/local/bin/dfsctl"},
	} {
		bin, err := crossBuild(ctx, b.pkg, b.name, arch)
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
	_, _ = fmt.Fprintf(exec.CmdOut, "ready: dfsbench run --remote --systems ...  (then dfsbench teardown)\n")
	return nil
}

// installPrereqs installs the shared load generator (fio — needed by every
// cell), the netcat used by waitPort, and every backend's server/client
// packages in one apt transaction. Front-loading here makes per-backend Setup a
// fast `command -v` no-op and guarantees fio exists before any run. (juicefs is
// not in apt — its recipe curl-installs it.)
func installPrereqs(ctx context.Context, ex exec.Executor, vm VM) error {
	// Core (fio load generator + waitPort's nc + the re-export servers) must
	// install. Competitor tools are best-effort (|| true) so a package missing
	// on this Ubuntu release disables just that backend, not the whole run
	// (s3ql, e.g., is absent from noble's repos).
	const core = "fio netcat-openbsd curl fuse3 nfs-kernel-server samba cifs-utils"
	const apt = "DEBIAN_FRONTEND=noninteractive apt-get install -y "
	cmd := "DEBIAN_FRONTEND=noninteractive apt-get update && " + apt + core +
		" ; " + apt + "s3fs rclone || true" +
		" ; " + apt + "s3ql || true"
	_, _ = fmt.Fprintln(exec.CmdOut, "installing prerequisites (fio + backend packages)…")
	_, err := ex.Run(ctx, vm.IP, remoteUser, cmd)
	return err
}

// NewTeardownCmd builds the `teardown` subcommand: terminate the bench VM
// recorded in .bench-vm.json.
func NewTeardownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "teardown",
		Short: "Terminate the bench VM recorded in .bench-vm.json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			vm, err := LoadVM()
			if err != nil {
				return err
			}
			p, err := newProvider(vm.Provider)
			if err != nil {
				return err
			}
			if err := p.Terminate(cmd.Context(), vm); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(exec.CmdOut, "terminated %s\n", vm.ServerID)
			return clearVMState()
		},
	}
}

// waitSSH blocks until the VM accepts an ssh command (60 × 5s).
func waitSSH(ctx context.Context, ex exec.Executor, vm VM) error {
	for i := 0; i < 60; i++ {
		if _, err := ex.Run(ctx, vm.IP, remoteUser, "true"); err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("ssh to %s never came up", vm.IP)
}

// detectArch maps the VM's `uname -m` to a Go GOARCH so cross-builds match the
// host whatever the provider hands back (amd64 x86_64, arm64 aarch64).
func detectArch(ctx context.Context, ex exec.Executor, vm VM) (string, error) {
	out, err := ex.Run(ctx, vm.IP, remoteUser, "uname -m")
	if err != nil {
		return "", fmt.Errorf("detect VM arch: %w", err)
	}
	switch m := strings.TrimSpace(string(out)); m {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported VM arch %q", m)
	}
}

// crossBuild builds a static linux binary for arch from pkg and returns its path.
func crossBuild(ctx context.Context, pkg, name, arch string) (string, error) {
	dir, err := os.MkdirTemp("", "dfsbench-build-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, name)
	c := osexec.CommandContext(ctx, "go", "build", "-o", bin, pkg)
	c.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-build %s: %w\n%s", pkg, err, out)
	}
	return bin, nil
}
