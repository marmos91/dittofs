//go:build e2e

package helpers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// CLIRunner executes dfsctl commands with JSON output for reliable parsing.
type CLIRunner struct {
	serverURL string
	token     string
	binary    string

	// xdgConfigHome, when non-empty, isolates this runner's dfsctl credential
	// storage. Every dfsctl invocation runs with XDG_CONFIG_HOME set to this
	// directory (set per-exec, not via a process-global env mutation), and the
	// runner's token extraction reads config.json from this same directory.
	// This keeps credential I/O confined to a per-test temp dir regardless of
	// call order. When empty, dfsctl inherits the ambient environment and
	// token extraction falls back to XDG_CONFIG_HOME / ~/.config.
	xdgConfigHome string
}

// NewCLIRunner creates a new CLI runner for the given server URL and auth token.
func NewCLIRunner(serverURL, token string) *CLIRunner {
	return &CLIRunner{
		serverURL: serverURL,
		token:     token,
	}
}

// Run executes dfsctl with --server, --token, and --output json prepended.
// Returns the raw output bytes and any error.
func (r *CLIRunner) Run(args ...string) ([]byte, error) {
	// Prepend standard args
	fullArgs := []string{"--output", "json"}
	if r.serverURL != "" {
		fullArgs = append(fullArgs, "--server", r.serverURL)
	}
	if r.token != "" {
		fullArgs = append(fullArgs, "--token", r.token)
	}
	fullArgs = append(fullArgs, args...)

	return r.execDfsctl(fullArgs...)
}

// RunRaw executes dfsctl without prepending standard args.
// Use this for commands that don't support all global flags (like login).
func (r *CLIRunner) RunRaw(args ...string) ([]byte, error) {
	return r.execDfsctl(args...)
}

// RunWithInput executes dfsctl and provides stdin input.
func (r *CLIRunner) RunWithInput(input string, args ...string) ([]byte, error) {
	// Prepend standard args
	fullArgs := []string{"--output", "json"}
	if r.serverURL != "" {
		fullArgs = append(fullArgs, "--server", r.serverURL)
	}
	if r.token != "" {
		fullArgs = append(fullArgs, "--token", r.token)
	}
	fullArgs = append(fullArgs, args...)

	return r.execDfsctlWithInput(input, fullArgs...)
}

// SetToken updates the authentication token.
func (r *CLIRunner) SetToken(token string) {
	r.token = token
}

// SetServerURL updates the server URL.
func (r *CLIRunner) SetServerURL(serverURL string) {
	r.serverURL = serverURL
}

// Token returns the current token.
func (r *CLIRunner) Token() string {
	return r.token
}

// ServerURL returns the current server URL.
func (r *CLIRunner) ServerURL() string {
	return r.serverURL
}

// execDfsctl runs the dfsctl binary with the given arguments.
func (r *CLIRunner) execDfsctl(args ...string) ([]byte, error) {
	binary := r.getBinary()
	cmd := exec.Command(binary, args...)
	cmd.Env = r.commandEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// Include stderr in error for better debugging
		return stdout.Bytes(), fmt.Errorf("dfsctl %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// execDfsctlWithInput runs the dfsctl binary with stdin input.
func (r *CLIRunner) execDfsctlWithInput(input string, args ...string) ([]byte, error) {
	binary := r.getBinary()
	cmd := exec.Command(binary, args...)
	cmd.Env = r.commandEnv()

	cmd.Stdin = strings.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("dfsctl %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// commandEnv returns the environment for a dfsctl child process. When the
// runner is isolated, XDG_CONFIG_HOME is overridden per-exec so credential I/O
// stays confined to the runner's temp dir; otherwise the ambient environment
// is inherited unchanged.
func (r *CLIRunner) commandEnv() []string {
	if r.xdgConfigHome == "" {
		return nil // nil means inherit the parent's environment
	}
	return append(os.Environ(), "XDG_CONFIG_HOME="+r.xdgConfigHome)
}

// configHomeDir returns the directory dfsctl uses to resolve
// <configHome>/dfsctl/config.json for this runner. It mirrors dfsctl's own
// resolution: the runner's isolated dir when set, else XDG_CONFIG_HOME, else
// ~/.config.
func (r *CLIRunner) configHomeDir() (string, error) {
	if r.xdgConfigHome != "" {
		return r.xdgConfigHome, nil
	}
	if env := os.Getenv("XDG_CONFIG_HOME"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home dir: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// getBinary returns the path to the dfsctl binary.
func (r *CLIRunner) getBinary() string {
	if r.binary != "" {
		return r.binary
	}

	// Check for dfsctl in PATH
	if path, err := exec.LookPath("dfsctl"); err == nil {
		r.binary = path
		return r.binary
	}

	// Look in project root
	projectRoot := findProjectRootForCLI()
	localBinary := filepath.Join(projectRoot, "dfsctl")
	if _, err := os.Stat(localBinary); err == nil {
		r.binary = localBinary
		return r.binary
	}

	// Build it
	cmd := exec.Command("go", "build", "-o", localBinary, "./cmd/dfsctl/")
	cmd.Dir = projectRoot
	if _, err := cmd.CombinedOutput(); err != nil {
		// Fall back to just "dfsctl" and let it fail later with better error
		r.binary = "dfsctl"
		return r.binary
	}

	r.binary = localBinary
	return r.binary
}

// findProjectRootForCLI locates the project root by looking for go.mod.
func findProjectRootForCLI() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// UniqueTestName generates a unique name for test resources.
// Format: {prefix}_{uuid8}
func UniqueTestName(prefix string) string {
	shortUUID := uuid.New().String()[:8]
	return fmt.Sprintf("%s_%s", prefix, shortUUID)
}

// LoginAsAdmin logs in as the admin user and returns a CLIRunner with the token.
// This uses the admin password from environment or prompts for it.
func LoginAsAdmin(t *testing.T, serverURL string) *CLIRunner {
	t.Helper()

	runner := NewCLIRunner(serverURL, "")

	// Isolate credential storage to a per-test temp dir so dfsctl login never
	// reads or writes the developer's real ~/.config/dfsctl/config.json. The
	// runner carries this dir and applies it as XDG_CONFIG_HOME on every dfsctl
	// invocation (per-exec, not via a process-global env mutation), and reads
	// the resulting config.json back from the same dir. t.TempDir is removed
	// when the test ends. Any runner derived from this one (e.g. via
	// LoginAsUser) inherits the same isolation.
	runner.xdgConfigHome = t.TempDir()

	// Try to login as admin
	// The login command doesn't support --output json, so we use RunRaw
	output, err := runner.RunRaw(
		"login",
		"--server", serverURL,
		"--username", "admin",
		"--password", getAdminPassword(t),
	)
	if err != nil {
		t.Fatalf("Failed to login as admin: %v\nOutput: %s", err, string(output))
	}

	// Extract token from the credentials file in the runner's isolated dir.
	token, err := runner.extractToken(serverURL)
	if err != nil {
		t.Fatalf("Failed to extract token after login: %v", err)
	}
	runner.SetToken(token)

	return runner
}

// getAdminPassword returns the admin password used for testing.
// Always returns the hardcoded test password to match the server helper.
// This ensures tests are self-contained and don't depend on environment variables.
func getAdminPassword(t *testing.T) string {
	t.Helper()
	// Must match the password set in server.go StartServerProcess
	return "adminpassword"
}

// GetAdminPassword returns the admin password used for testing.
// Exported version for use in tests that need direct password access.
func GetAdminPassword() string {
	return "adminpassword"
}

// extractToken reads the auth token for serverURL from the dfsctl credentials
// file in this runner's config dir. For an isolated runner that is the per-test
// temp dir, so token extraction stays paired with the dir the login wrote to.
func (r *CLIRunner) extractToken(serverURL string) (string, error) {
	configHome, err := r.configHomeDir()
	if err != nil {
		return "", err
	}
	return parseTokenFromCredentialsFile(configHome, serverURL)
}

// ParseJSONResponse parses a JSON response into the given struct.
func ParseJSONResponse(output []byte, v interface{}) error {
	if err := json.Unmarshal(output, v); err != nil {
		return fmt.Errorf("failed to parse JSON response: %w\nraw: %s", err, string(output))
	}
	return nil
}

// RunDfs executes the dfs (server) binary with the given arguments.
// This is useful for commands like `dfs status`.
func RunDfs(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()

	binary := findDfsBinaryForCLI(t)
	cmd := exec.Command(binary, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("dfs %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// findDfsBinaryForCLI locates the dfs binary, similar to findDfsBinary but without testing.T dependency.
func findDfsBinaryForCLI(t *testing.T) string {
	t.Helper()

	// Check for dfs in PATH
	if path, err := exec.LookPath("dfs"); err == nil {
		return path
	}

	// Check for dfs in project root
	projectRoot := findProjectRootForCLI()
	localBinary := filepath.Join(projectRoot, "dfs")
	if _, err := os.Stat(localBinary); err == nil {
		return localBinary
	}

	// Try to build it
	t.Log("Building dfs binary...")
	cmd := exec.Command("go", "build", "-o", localBinary, "./cmd/dfs/")
	cmd.Dir = projectRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build dfs: %v\n%s", err, output)
	}

	return localBinary
}
