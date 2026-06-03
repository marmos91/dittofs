package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// SSHConfig configures the system-ssh-backed Executor. It mirrors the knobs the
// bash scripts used (an explicit key, agent disabled) so behaviour is identical
// to scripts/run-bench.sh, just driven from Go.
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

// pulumiStackReader runs `pulumi stack output --json` in a working directory
// (the Pulumi project root, e.g. bench/infra).
type pulumiStackReader struct {
	// WorkDir is the Pulumi project directory. Empty uses the process cwd.
	WorkDir string
	// Passphrase, when set, is exported as PULUMI_CONFIG_PASSPHRASE so a
	// passphrase-protected stack can be read non-interactively. It is passed via
	// the child environment, never as an argument.
	Passphrase string
}

// NewPulumiStackReader returns a StackReader backed by the pulumi CLI.
func NewPulumiStackReader(workDir, passphrase string) StackReader {
	return &pulumiStackReader{WorkDir: workDir, Passphrase: passphrase}
}

func (r *pulumiStackReader) Outputs(ctx context.Context, stack string) (map[string]string, error) {
	c := exec.CommandContext(ctx, "pulumi", "stack", "output", "--json", "--stack", stack)
	if r.WorkDir != "" {
		c.Dir = r.WorkDir
	}
	if r.Passphrase != "" {
		c.Env = append(c.Environ(), "PULUMI_CONFIG_PASSPHRASE="+r.Passphrase)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("pulumi stack output (stack %q): %w: %s", stack, err, strings.TrimSpace(stderr.String()))
	}
	// Outputs are a JSON object; coerce every value to its string form so the
	// flat map[string]string contract holds regardless of the export's type.
	var raw map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parse pulumi outputs: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = stringifyOutput(v)
	}
	return out, nil
}

// stringifyOutput renders a Pulumi output value as a string. Strings pass
// through; numbers/bools use their natural form; anything else is JSON-encoded.
func stringifyOutput(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// Pulumi exports ints as JSON numbers; render without a trailing .0.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
