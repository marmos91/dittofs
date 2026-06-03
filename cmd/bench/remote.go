package main

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/bench/orchestrator"
	"github.com/marmos91/dittofs/bench/remote"
	"github.com/spf13/cobra"
)

// remote subcommand: the Go replacement for scripts/run-bench.sh +
// bench/scripts/run-all.sh. It reads the Pulumi stack outputs, scp's a prebuilt
// dfsbench binary to the server, runs `orchestrate` over SSH, and pulls the
// result JSON back. SSH targets the PUBLIC IP; the bench serves on the PRIVATE
// IP (failure mode B5). Provisioning the infra itself stays in Pulumi
// (bench/infra) — this drives an already-provisioned host.
var (
	remStack       string
	remPulumiDir   string
	remPassphrase  string
	remUser        string
	remPrivateIP   string
	remSSHKey      string
	remDisableAgnt bool
	remBinary      string
	remRemoteBin   string
	remManifest    string
	remRemoteMan   string
	remRemoteOut   string
	remLocalOut    string
	remRunID       string
	remTimestamp   string
	remGitSHA      string
	remDryRun      bool
	remSummary     bool
)

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Drive a benchmark on a remote (Scaleway) host over SSH and collect results",
	Long: `Drives a DittoFS benchmark on an already-provisioned remote host and
collects the versioned result JSON back — the Go replacement for
scripts/run-bench.sh and bench/scripts/run-all.sh.

It reads the bench server's public IP (for SSH) and private-network IP (for the
mount) from a Pulumi stack's outputs, scp's the prebuilt dfsbench binary to the
host, runs 'orchestrate' over SSH, and pulls the result document down — then
verifies its schema_version.

SSH ALWAYS uses the public IP; the benchmark mounts/serves over the
private-network IP only. The two are kept distinct so a bench is never run over
the public path.

This drives real cloud infrastructure. --dry-run resolves the target and prints
the planned actions WITHOUT touching the host, so the wiring is verifiable
without credentials. Provisioning the VMs themselves remains 'pulumi up' in
bench/infra (see bench/README.md); this command drives an existing host.

Required env / setup:
  - Pulumi stack provisioned (cd bench/infra && pulumi up --stack bench)
  - SSH access to the server's public IP (--ssh-key or an agent)
  - A linux/amd64 dfsbench build to push (--binary), e.g.
      GOOS=linux GOARCH=amd64 go build -o dfsbench.linux ./cmd/bench`,
	RunE: runRemote,
}

func init() {
	f := remoteCmd.Flags()
	f.StringVar(&remStack, "stack", "bench", "Pulumi stack to read outputs from")
	f.StringVar(&remPulumiDir, "pulumi-dir", "bench/infra", "Pulumi project directory")
	f.StringVar(&remPassphrase, "pulumi-passphrase", "", "PULUMI_CONFIG_PASSPHRASE for a protected stack (passed via env, never logged)")
	f.StringVar(&remUser, "ssh-user", "root", "SSH login user")
	f.StringVar(&remPrivateIP, "private-ip", "", "override the server's private-network IP (when not exported by the stack)")
	f.StringVar(&remSSHKey, "ssh-key", "", "SSH private key path (-i); empty uses the agent")
	f.BoolVar(&remDisableAgnt, "ssh-no-agent", false, "disable the SSH agent (-o IdentityAgent=none)")
	f.StringVar(&remBinary, "binary", "", "local prebuilt dfsbench (linux/amd64) to push; empty assumes it is already installed")
	f.StringVar(&remRemoteBin, "remote-binary", "/usr/local/bin/dfsbench", "absolute path the bench binary lives at on the host")
	f.StringVar(&remManifest, "manifest", "", "local manifest JSON to push and run; empty uses the built-in default")
	f.StringVar(&remRemoteMan, "remote-manifest", "/tmp/bench-manifest.json", "path the manifest lands at on the host")
	f.StringVar(&remRemoteOut, "remote-out", "/tmp/bench-result.json", "path the result JSON is written at on the host")
	f.StringVar(&remLocalOut, "out", "remote-result.json", "local path to write the fetched result JSON")
	f.StringVar(&remRunID, "run-id", "", "run identifier to stamp (default: remote binary derives one)")
	f.StringVar(&remTimestamp, "timestamp", "", "run timestamp RFC3339 to stamp (default: remote now)")
	f.StringVar(&remGitSHA, "git-sha", "", "git SHA to stamp (default: remote build VCS revision)")
	f.BoolVar(&remDryRun, "dry-run", false, "resolve the target and print the plan WITHOUT touching the host")
	f.BoolVar(&remSummary, "summary", false, "print the result summary table after a successful run")

	rootCmd.AddCommand(remoteCmd)
}

// Seams for testing: the real run uses pulumi/ssh; tests swap in fakes so the
// command's dry-run and wiring are exercised without cloud access.
var (
	newStackReader = func() remote.StackReader { return remote.NewPulumiStackReader(remPulumiDir, remPassphrase) }
	newExecutor    = func() remote.Executor {
		return remote.NewSSHExecutor(remote.SSHConfig{
			KeyPath:      remSSHKey,
			DisableAgent: remDisableAgnt,
			ExtraOpts:    []string{"StrictHostKeyChecking=accept-new"},
		})
	}
)

func runRemote(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	reader := newStackReader()
	// Mounting over the private IP is required, but in --dry-run we still want a
	// useful plan even before the private IP is wired, so we only hard-require
	// it for a real run.
	target, err := remote.ResolveTarget(ctx, reader, remStack, remUser, remPrivateIP, !remDryRun)
	if err != nil {
		return err
	}

	plan := remote.Plan{
		Target:             target,
		LocalBinary:        remBinary,
		RemoteBinary:       remRemoteBin,
		RemoteResultPath:   remRemoteOut,
		LocalResultPath:    remLocalOut,
		ManifestPath:       remManifest,
		RemoteManifestPath: remRemoteMan,
		RunID:              remRunID,
		Timestamp:          remTimestamp,
		GitSHA:             remGitSHA,
	}
	if err := plan.Validate(); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if remDryRun {
		_, _ = fmt.Fprintf(out, "DRY RUN — no host action taken\n")
		_, _ = fmt.Fprintf(out, "ssh target (public):   %s@%s\n", target.User, target.PublicIP)
		_, _ = fmt.Fprintf(out, "bench mount (private):  %s\n", privateOrUnset(target.PrivateIP))
		if plan.LocalBinary != "" {
			_, _ = fmt.Fprintf(out, "push binary:            %s -> %s\n", plan.LocalBinary, plan.RemoteBinary)
		}
		if plan.ManifestPath != "" {
			_, _ = fmt.Fprintf(out, "push manifest:          %s -> %s\n", plan.ManifestPath, plan.RemoteManifestPath)
		}
		_, _ = fmt.Fprintf(out, "remote command:         %s\n", plan.BenchCommand())
		_, _ = fmt.Fprintf(out, "fetch result:           %s -> %s\n", plan.RemoteResultPath, plan.LocalResultPath)
		return nil
	}

	doc, err := remote.New(newExecutor()).Run(ctx, plan)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "result written to %s (schema_version=%d outcome=%s)\n",
		plan.LocalResultPath, doc.SchemaVersion, doc.Outcome)
	if remSummary {
		orchestrator.WriteSummary(cmd.ErrOrStderr(), doc)
	}
	if doc.Outcome != orchestrator.OutcomeCompleted {
		return fmt.Errorf("remote run outcome %s", doc.Outcome)
	}
	return nil
}

func privateOrUnset(ip string) string {
	if ip == "" {
		return "(unset — required for a real run)"
	}
	return ip
}
