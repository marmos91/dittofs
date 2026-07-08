package exec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// CmdOut is where commands write their human-facing output. A package var keeps
// the plumbing out of every function signature; tests point it at a buffer.
var CmdOut io.Writer = os.Stdout

// Executor runs commands and moves files on a remote host. The dfsbench
// harness drives a single disposable benchmark VM through it (ssh/scp today);
// it is deliberately tiny so a test double or a future transport can satisfy it.
type Executor interface {
	Run(ctx context.Context, host, user, cmd string) ([]byte, error)
	Push(ctx context.Context, localPath, host, user, remotePath string) error
	Pull(ctx context.Context, host, user, remotePath, localPath string) error
}

// SSHConfig configures the system-ssh-backed Executor. It mirrors the knobs the
// bash scripts used (an explicit key, agent disabled) so behaviour is identical,
// just driven from Go.
type SSHConfig struct {
	// KeyPath is the private key passed via -i. Empty uses the agent/default.
	KeyPath string
	// DisableAgent adds -o IdentityAgent=none (the 1Password-agent workaround
	// the bash scripts carried). Default false.
	DisableAgent bool
	// ExtraOpts are appended verbatim as ssh -o options (e.g.
	// "StrictHostKeyChecking=accept-new").
	ExtraOpts []string
}

// sshExecutor shells out to the system ssh/scp. It carries NO secrets: command
// strings are operator-supplied and secret values travel via the remote
// environment, set by the caller's command, never logged here.
type sshExecutor struct {
	cfg SSHConfig
}

// NewSSHExecutor returns an Executor backed by the system ssh/scp binaries.
func NewSSHExecutor(cfg SSHConfig) Executor { return &sshExecutor{cfg: cfg} }

// baseArgs are the shared -i / -o options for both ssh and scp.
func (e *sshExecutor) baseArgs() []string {
	var args []string
	if e.cfg.DisableAgent {
		args = append(args, "-o", "IdentityAgent=none")
	}
	if e.cfg.KeyPath != "" {
		args = append(args, "-i", e.cfg.KeyPath)
	}
	for _, o := range e.cfg.ExtraOpts {
		args = append(args, "-o", o)
	}
	return args
}

func (e *sshExecutor) Run(ctx context.Context, host, user, cmd string) ([]byte, error) {
	args := append(e.baseArgs(), fmt.Sprintf("%s@%s", user, host), cmd)
	c := exec.CommandContext(ctx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		// Surface stderr but NOT the full command (it could echo a remote env
		// assignment); the operator already knows what they asked to run.
		return stdout.Bytes(), fmt.Errorf("ssh %s@%s failed: %w: %s",
			user, host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (e *sshExecutor) Push(ctx context.Context, localPath, host, user, remotePath string) error {
	args := append(e.baseArgs(), localPath, fmt.Sprintf("%s@%s:%s", user, host, remotePath))
	return runScp(ctx, args, "push", localPath, remotePath)
}

func (e *sshExecutor) Pull(ctx context.Context, host, user, remotePath, localPath string) error {
	args := append(e.baseArgs(), fmt.Sprintf("%s@%s:%s", user, host, remotePath), localPath)
	return runScp(ctx, args, "pull", remotePath, localPath)
}

func runScp(ctx context.Context, args []string, dir, from, to string) error {
	c := exec.CommandContext(ctx, "scp", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("scp %s %s -> %s: %w: %s", dir, from, to, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
