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

// CLIRunner executes dittofsctl commands with JSON output for reliable parsing.
type CLIRunner struct {
	serverURL string
	token     string
	binary    string
}

// NewCLIRunner creates a new CLI runner for the given server URL and auth token.
func NewCLIRunner(serverURL, token string) *CLIRunner {
	return &CLIRunner{
		serverURL: serverURL,
		token:     token,
	}
}

// Run executes dittofsctl with --server, --token, and --output json prepended.
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

	return r.execDittofsctl(fullArgs...)
}

// RunRaw executes dittofsctl without prepending standard args.
// Use this for commands that don't support all global flags (like login).
func (r *CLIRunner) RunRaw(args ...string) ([]byte, error) {
	return r.execDittofsctl(args...)
}

// RunWithInput executes dittofsctl and provides stdin input.
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

	return r.execDittofsctlWithInput(input, fullArgs...)
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

// execDittofsctl runs the dittofsctl binary with the given arguments.
func (r *CLIRunner) execDittofsctl(args ...string) ([]byte, error) {
	binary := r.getBinary()
	cmd := exec.Command(binary, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// Include stderr in error for better debugging
		return stdout.Bytes(), fmt.Errorf("dittofsctl %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// execDittofsctlWithInput runs the dittofsctl binary with stdin input.
func (r *CLIRunner) execDittofsctlWithInput(input string, args ...string) ([]byte, error) {
	binary := r.getBinary()
	cmd := exec.Command(binary, args...)

	cmd.Stdin = strings.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("dittofsctl %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// getBinary returns the path to the dittofsctl binary.
func (r *CLIRunner) getBinary() string {
	if r.binary != "" {
		return r.binary
	}

	// Check for dittofsctl in PATH
	if path, err := exec.LookPath("dittofsctl"); err == nil {
		r.binary = path
		return r.binary
	}

	// Look in project root
	projectRoot := findProjectRootForCLI()
	localBinary := filepath.Join(projectRoot, "dittofsctl")
	if _, err := os.Stat(localBinary); err == nil {
		r.binary = localBinary
		return r.binary
	}

	// Build it
	cmd := exec.Command("go", "build", "-o", localBinary, "./cmd/dittofsctl/")
	cmd.Dir = projectRoot
	if _, err := cmd.CombinedOutput(); err != nil {
		// Fall back to just "dittofsctl" and let it fail later with better error
		r.binary = "dittofsctl"
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

	// Extract token from the login response or credentials file
	token := extractTokenFromLogin(t, serverURL)
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

// extractTokenFromLogin extracts the auth token after login.
// This reads from the credentials file that dittofsctl creates.
func extractTokenFromLogin(t *testing.T, serverURL string) string {
	t.Helper()

	// Get credentials file path (matches internal/cli/credentials/store.go)
	// Uses XDG_CONFIG_HOME or ~/.config
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("Failed to get home dir: %v", err)
		}
		configHome = filepath.Join(home, ".config")
	}

	credFile := filepath.Join(configHome, "dittofsctl", "config.json")
	data, err := os.ReadFile(credFile)
	if err != nil {
		t.Fatalf("Failed to read credentials file: %v", err)
	}

	// Parse the credentials file
	var creds map[string]interface{}
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatalf("Failed to parse credentials file: %v", err)
	}

	// The credentials file stores contexts with server URLs
	// Find the matching context
	contexts, ok := creds["contexts"].(map[string]interface{})
	if !ok {
		t.Fatalf("No contexts found in credentials file")
	}

	for _, ctx := range contexts {
		ctxMap, ok := ctx.(map[string]interface{})
		if !ok {
			continue
		}
		// Field name is "server_url" in JSON (snake_case)
		if ctxMap["server_url"] == serverURL {
			if token, ok := ctxMap["access_token"].(string); ok {
				return token
			}
		}
	}

	t.Fatalf("No token found for server %s in credentials file", serverURL)
	return ""
}

// ParseJSONResponse parses a JSON response into the given struct.
func ParseJSONResponse(output []byte, v interface{}) error {
	if err := json.Unmarshal(output, v); err != nil {
		return fmt.Errorf("failed to parse JSON response: %w\nraw: %s", err, string(output))
	}
	return nil
}

// RunDittofs executes the dittofs (server) binary with the given arguments.
// This is useful for commands like `dittofs status`.
func RunDittofs(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()

	binary := findDittofsBinaryForCLI(t)
	cmd := exec.Command(binary, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("dittofs %s failed: %w\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// findDittofsBinaryForCLI locates the dittofs binary, similar to findDittofsBinary but without testing.T dependency.
func findDittofsBinaryForCLI(t *testing.T) string {
	t.Helper()

	// Check for dittofs in PATH
	if path, err := exec.LookPath("dittofs"); err == nil {
		return path
	}

	// Check for dittofs in project root
	projectRoot := findProjectRootForCLI()
	localBinary := filepath.Join(projectRoot, "dittofs")
	if _, err := os.Stat(localBinary); err == nil {
		return localBinary
	}

	// Try to build it
	t.Log("Building dittofs binary...")
	cmd := exec.Command("go", "build", "-o", localBinary, "./cmd/dittofs/")
	cmd.Dir = projectRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build dittofs: %v\n%s", err, output)
	}

	return localBinary
}
