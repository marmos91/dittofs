package cloud

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// NewSetupCmd builds the `setup` subcommand: provision a disposable SCW VM and
// push the dfsbench binary (plus dfs/dfsctl) to it.
func NewSetupCmd() *cobra.Command {
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
	_, _ = fmt.Fprintf(exec.CmdOut, "provisioned %s (%s)\n", vm.ServerID, vm.IP)

	ex := exec.NewSSHExecutor(SSHCfg())
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
			if err := terminateVM(cmd.Context(), vm); err != nil {
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

// crossBuild builds a static linux/amd64 binary from pkg and returns its path.
func crossBuild(ctx context.Context, pkg, name string) (string, error) {
	dir, err := os.MkdirTemp("", "dfsbench-build-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, name)
	c := osexec.CommandContext(ctx, "go", "build", "-o", bin, pkg)
	c.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-build %s: %w\n%s", pkg, err, out)
	}
	return bin, nil
}
