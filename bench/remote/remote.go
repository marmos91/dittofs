// Package remote drives a DittoFS benchmark run on a remote host and collects
// the versioned result document back. It is the Go replacement for the
// scripts/run-bench.sh + bench/scripts/run-all.sh bash orchestration: read the
// Pulumi stack outputs, copy the prebuilt dfs/dfsbench binary to the server,
// run the bench over SSH, and fetch the result JSON.
//
// The side-effecting pieces — SSH command execution, scp file transfer, and
// reading Pulumi stack outputs — sit behind small interfaces (Executor,
// StackReader) so the orchestration logic is unit-testable with fakes and the
// real implementations (exec.go) shell out to the system ssh/scp/pulumi the
// same way the bash scripts did. No live cloud access happens in this package's
// pure logic; provisioning is gated by the caller.
//
// Network policy mirrors the baseline-README requirement (failure mode B5):
// SSH always targets the PUBLIC IP, but the benchmark mounts/serves over the
// PRIVATE-network IP. The two are kept in distinct fields so they can never be
// transposed by accident.
package remote

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/bench/orchestrator"
)

// Executor runs commands and transfers files on a remote host. The real
// implementation (NewSSHExecutor) shells out to ssh/scp; tests inject a fake.
//
// Implementations MUST NOT log command strings that may carry secrets (S3
// keys, admin passwords). Callers pass secrets via the remote process
// environment, not the command line, and Run echoes neither.
type Executor interface {
	// Run executes cmd on host as user over SSH and returns its combined stdout.
	// A non-zero exit is returned as an error with stderr context.
	Run(ctx context.Context, host, user, cmd string) ([]byte, error)
	// Push copies a local file to remotePath on host.
	Push(ctx context.Context, localPath, host, user, remotePath string) error
	// Pull copies remotePath on host down to localPath.
	Pull(ctx context.Context, host, user, remotePath, localPath string) error
}

// StackReader reads the outputs of an IaC stack (Pulumi). The real
// implementation (NewPulumiStackReader) runs `pulumi stack output --json`.
type StackReader interface {
	// Outputs returns the named stack's outputs as a flat string map.
	Outputs(ctx context.Context, stack string) (map[string]string, error)
}

// Target is the resolved connection + addressing for a remote bench host.
type Target struct {
	// PublicIP is used for SSH/scp ONLY. Never used for the bench mount.
	PublicIP string
	// PrivateIP is the private-network address the benchmark serves/mounts on.
	// Empty is rejected by Validate when RequirePrivate is set.
	PrivateIP string
	// User is the SSH login (e.g. "root").
	User string
}

// Validate checks a target is usable. requirePrivate enforces that a
// private-network IP was resolved (the B5 requirement) before any mount runs.
func (t Target) Validate(requirePrivate bool) error {
	if t.PublicIP == "" {
		return fmt.Errorf("remote target: public IP (for SSH) is required")
	}
	if t.User == "" {
		return fmt.Errorf("remote target: SSH user is required")
	}
	if requirePrivate && t.PrivateIP == "" {
		return fmt.Errorf("remote target: private-network IP is required for mounts " +
			"(SSH uses the public IP, the bench must not) — check the Pulumi stack outputs")
	}
	return nil
}

// Plan is everything the orchestrator needs to drive one remote bench run.
type Plan struct {
	Target Target

	// LocalBinary is the path to the prebuilt dfsbench binary to push (must be a
	// linux/amd64 build for a Scaleway VM). Empty means "already installed";
	// then RemoteBinary is invoked as-is.
	LocalBinary string
	// RemoteBinary is the absolute path the bench binary lives at on the host.
	RemoteBinary string
	// RemoteResultPath is where the bench writes its JSON on the host before we
	// pull it down.
	RemoteResultPath string
	// LocalResultPath is where the fetched JSON lands locally.
	LocalResultPath string

	// ManifestPath, when set, is a local manifest file pushed alongside the
	// binary and passed to `orchestrate --manifest`. Empty uses the binary's
	// built-in default manifest.
	ManifestPath string
	// RemoteManifestPath is where the manifest lands on the host.
	RemoteManifestPath string

	// RunID / Timestamp / GitSHA stamp the run for reproducibility, exactly like
	// the local orchestrate path. Empty lets the remote binary fill defaults.
	RunID     string
	Timestamp string
	GitSHA    string
}

// Validate checks the plan is internally consistent before any remote action.
func (p Plan) Validate() error {
	if err := p.Target.Validate(false); err != nil {
		return err
	}
	if p.RemoteBinary == "" {
		return fmt.Errorf("plan: remote binary path is required")
	}
	if p.RemoteResultPath == "" {
		return fmt.Errorf("plan: remote result path is required")
	}
	if p.LocalResultPath == "" {
		return fmt.Errorf("plan: local result path is required")
	}
	if p.LocalBinary != "" {
		if _, err := os.Stat(p.LocalBinary); err != nil {
			return fmt.Errorf("plan: local binary %q: %w", p.LocalBinary, err)
		}
	}
	if p.ManifestPath != "" {
		if _, err := os.Stat(p.ManifestPath); err != nil {
			return fmt.Errorf("plan: manifest %q: %w", p.ManifestPath, err)
		}
		if p.RemoteManifestPath == "" {
			return fmt.Errorf("plan: remote manifest path required when --manifest is set")
		}
	}
	return nil
}

// MountIPEnv is the environment variable the remote bench reads to learn which
// address to mount/serve on. It always carries the PRIVATE-network IP — the
// public IP is for SSH transport only and is never exported here.
const MountIPEnv = "DITTOFS_BENCH_MOUNT_IP"

// BenchCommand assembles the remote `orchestrate` invocation for this plan. It
// is exported and pure so tests can assert the exact command without running
// anything.
//
// When a private-network IP is resolved it is exported as MountIPEnv so the
// remote bench mounts/serves over the private path (the public IP, used only
// for the SSH transport, is never embedded here). The current `orchestrate`
// workloads run in-process on the host and do not yet mount; the env var is the
// wiring point a mount-driven runner consumes.
func (p Plan) BenchCommand() string {
	var parts []string
	if p.Target.PrivateIP != "" {
		parts = append(parts, MountIPEnv+"="+shellQuote(p.Target.PrivateIP))
	}
	parts = append(parts, p.RemoteBinary, "orchestrate", "--out", shellQuote(p.RemoteResultPath))
	if p.RemoteManifestPath != "" {
		parts = append(parts, "--manifest", shellQuote(p.RemoteManifestPath))
	}
	if p.RunID != "" {
		parts = append(parts, "--run-id", shellQuote(p.RunID))
	}
	if p.Timestamp != "" {
		parts = append(parts, "--timestamp", shellQuote(p.Timestamp))
	}
	if p.GitSHA != "" {
		parts = append(parts, "--git-sha", shellQuote(p.GitSHA))
	}
	return strings.Join(parts, " ")
}

// Orchestrator drives a remote bench run end-to-end via injected side-effect
// ports, so the whole flow is exercised in tests with fakes.
type Orchestrator struct {
	Exec Executor
}

// New returns an Orchestrator over the given Executor.
func New(exec Executor) *Orchestrator { return &Orchestrator{Exec: exec} }

// Run executes the plan: push the binary (and manifest, if any), run the bench
// over SSH against the PUBLIC IP, pull the result JSON, and decode+verify it.
// The returned Document has already passed the schema version check.
func (o *Orchestrator) Run(ctx context.Context, p Plan) (*orchestrator.Document, error) {
	if o == nil || o.Exec == nil {
		return nil, fmt.Errorf("remote.Orchestrator: executor is nil")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	host, user := p.Target.PublicIP, p.Target.User

	if p.LocalBinary != "" {
		if err := o.Exec.Push(ctx, p.LocalBinary, host, user, p.RemoteBinary); err != nil {
			return nil, fmt.Errorf("push binary: %w", err)
		}
		// chmod so the freshly-scp'd binary is executable.
		if _, err := o.Exec.Run(ctx, host, user, "chmod +x "+shellQuote(p.RemoteBinary)); err != nil {
			return nil, fmt.Errorf("chmod binary: %w", err)
		}
	}
	if p.ManifestPath != "" {
		if err := o.Exec.Push(ctx, p.ManifestPath, host, user, p.RemoteManifestPath); err != nil {
			return nil, fmt.Errorf("push manifest: %w", err)
		}
	}

	if _, err := o.Exec.Run(ctx, host, user, p.BenchCommand()); err != nil {
		return nil, fmt.Errorf("run bench: %w", err)
	}

	if err := o.Exec.Pull(ctx, host, user, p.RemoteResultPath, p.LocalResultPath); err != nil {
		return nil, fmt.Errorf("pull result: %w", err)
	}
	doc, err := orchestrator.DecodeFile(p.LocalResultPath)
	if err != nil {
		return nil, fmt.Errorf("fetched result: %w", err)
	}
	return doc, nil
}

// shellQuote wraps s in single quotes for safe interpolation into a remote
// shell command, escaping any embedded single quotes. Paths and run metadata
// pass through here; secrets never do (they travel via the remote environment).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
