//go:build e2e

package helpers

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ServerProcess manages a DittoFS server subprocess for E2E testing.
// It provides methods to start the server, check health, send signals, and stop gracefully.
type ServerProcess struct {
	cmd           *exec.Cmd
	pidFile       string
	apiPort       int
	logFile       string
	stateDir      string
	configFile    string
	process       *os.Process
	logFileHandle *os.File
}

// HealthResponse represents the /health endpoint response structure.
type HealthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp,omitempty"`
	Error     string `json:"error,omitempty"`
	Data      struct {
		Service   string `json:"service,omitempty"`
		StartedAt string `json:"started_at,omitempty"`
		Uptime    string `json:"uptime,omitempty"`
		UptimeSec int64  `json:"uptime_sec,omitempty"`
	} `json:"data,omitempty"`
}

// ReadinessResponse represents the /health/ready endpoint response structure.
type ReadinessResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp,omitempty"`
	Error     string `json:"error,omitempty"`
	Data      struct {
		Shares         int `json:"shares,omitempty"`
		MetadataStores int `json:"metadata_stores,omitempty"`
		Adapters       struct {
			Running int      `json:"running,omitempty"`
			Types   []string `json:"types,omitempty"`
		} `json:"adapters,omitempty"`
	} `json:"data,omitempty"`
}

// FindFreePort finds an available TCP port by binding to :0 and reading the assigned port.
func FindFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port
}

// StartServerProcess starts a DittoFS server in foreground mode with a custom config.
// It polls /health/ready until ready (5-second timeout per CONTEXT.md).
// Uses t.TempDir() for state directory (pid file, log file).
func StartServerProcess(t *testing.T, configPath string) *ServerProcess {
	t.Helper()

	stateDir := t.TempDir()
	apiPort := FindFreePort(t)

	// Create a modified config with our API port
	configWithPort := createConfigWithAPIPort(t, configPath, apiPort, stateDir)

	pidFile := filepath.Join(stateDir, "dfs.pid")
	logFile := filepath.Join(stateDir, "dfs.log")

	// Find the dfs binary
	dfsPath := findDfsBinary(t)

	// Start server in foreground mode
	cmd := exec.Command(dfsPath, "start", "--foreground",
		"--config", configWithPort,
		"--pid-file", pidFile,
		"--log-file", logFile)

	// Set admin password for server initialization
	// Always use a known good password for tests (min 8 chars required)
	adminPassword := "adminpassword"

	// Build environment for subprocess, filtering out any existing password env vars
	// to avoid conflicts with duplicate entries
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DITTOFS_ADMIN_PASSWORD=") &&
			!strings.HasPrefix(e, "DITTOFS_ADMIN_INITIAL_PASSWORD=") {
			env = append(env, e)
		}
	}
	env = append(env, "DITTOFS_ADMIN_INITIAL_PASSWORD="+adminPassword)
	cmd.Env = env

	// Redirect stdout/stderr to log file
	logFileHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("Failed to create log file: %v", err)
	}

	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	// Start the process
	if err := cmd.Start(); err != nil {
		_ = logFileHandle.Close()
		t.Fatalf("Failed to start dfs server: %v", err)
	}

	sp := &ServerProcess{
		cmd:           cmd,
		pidFile:       pidFile,
		apiPort:       apiPort,
		logFile:       logFile,
		stateDir:      stateDir,
		configFile:    configWithPort,
		process:       cmd.Process,
		logFileHandle: logFileHandle,
	}

	// Wait for server to be ready
	if err := sp.WaitReady(5 * time.Second); err != nil {
		// Dump logs on failure
		sp.dumpLogs(t)
		sp.ForceKill()
		t.Fatalf("Server failed to become ready: %v", err)
	}

	return sp
}

// WaitReady polls the /health endpoint until the server is ready or timeout.
// We use /health (liveness) instead of /health/ready (readiness) because
// readiness requires adapters to be running, which may conflict with ports
// in parallel test runs. For server lifecycle tests, we just need the API
// server to be responding.
func (sp *ServerProcess) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", sp.apiPort)

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}

		lastErr = fmt.Errorf("health check returned %d: %s", resp.StatusCode, string(body))
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("server not healthy after %v: %w", timeout, lastErr)
}

// CheckHealth performs a GET /health and parses the response.
func (sp *ServerProcess) CheckHealth() (*HealthResponse, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", sp.apiPort)

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var healthResp HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		return nil, fmt.Errorf("failed to decode health response: %w", err)
	}

	return &healthResp, nil
}

// CheckReady performs a GET /health/ready and parses the response.
func (sp *ServerProcess) CheckReady() (*ReadinessResponse, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health/ready", sp.apiPort)

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("readiness check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var readyResp ReadinessResponse
	if err := json.NewDecoder(resp.Body).Decode(&readyResp); err != nil {
		return nil, fmt.Errorf("failed to decode readiness response: %w", err)
	}

	return &readyResp, nil
}

// SendSignal sends a signal to the server process.
func (sp *ServerProcess) SendSignal(sig syscall.Signal) error {
	if sp.process == nil {
		return fmt.Errorf("no process to signal")
	}
	return sp.process.Signal(sig)
}

// WaitForExit waits for the process to exit within the timeout.
func (sp *ServerProcess) WaitForExit(timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := sp.process.Wait()
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("process did not exit within %v", timeout)
	}
}

// ForceKill terminates the server process.
// It first attempts graceful shutdown (SIGTERM) to allow NFS/SMB connections
// to close cleanly, then falls back to SIGKILL if the process doesn't exit.
func (sp *ServerProcess) ForceKill() {
	if sp.process == nil {
		return
	}

	// Try graceful shutdown first to allow NFS client connections to close cleanly.
	// Without this, macOS shows "Server connections interrupted" dialogs.
	_ = sp.process.Signal(syscall.SIGTERM)

	// Wait up to 2 seconds for graceful exit
	done := make(chan struct{})
	go func() {
		_, _ = sp.process.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Process exited gracefully
	case <-time.After(2 * time.Second):
		// Graceful shutdown timed out, force kill
		_ = sp.process.Kill()
		<-done
	}

	// Close log file handle to avoid descriptor leak
	if sp.logFileHandle != nil {
		_ = sp.logFileHandle.Close()
		sp.logFileHandle = nil
	}
}

// StopGracefully sends SIGTERM and waits for clean exit.
func (sp *ServerProcess) StopGracefully() error {
	if err := sp.SendSignal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send SIGTERM: %w", err)
	}
	return sp.WaitForExit(10 * time.Second)
}

// APIPort returns the API port for client connections.
func (sp *ServerProcess) APIPort() int {
	return sp.apiPort
}

// APIURL returns the full API URL for the server.
func (sp *ServerProcess) APIURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", sp.apiPort)
}

// LogFile returns the path to the server log file.
func (sp *ServerProcess) LogFile() string {
	return sp.logFile
}

// PidFile returns the path to the server PID file.
func (sp *ServerProcess) PidFile() string {
	return sp.pidFile
}

// ConfigFile returns the path to the server config file.
func (sp *ServerProcess) ConfigFile() string {
	return sp.configFile
}

// ProcessRunning checks if the server process is still running.
func (sp *ServerProcess) ProcessRunning() bool {
	if sp.process == nil {
		return false
	}
	// Sending signal 0 checks if process exists without actually sending a signal
	err := sp.process.Signal(syscall.Signal(0))
	return err == nil
}

// PID returns the process ID of the server.
// Returns 0 if the process is not running.
func (sp *ServerProcess) PID() int {
	if sp.process == nil {
		return 0
	}
	return sp.process.Pid
}

// dumpLogs prints the log file contents to help debug failures.
func (sp *ServerProcess) dumpLogs(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile(sp.logFile)
	if err != nil {
		t.Logf("Could not read log file: %v", err)
		return
	}

	t.Logf("Server logs:\n%s", string(content))
}

// DumpLogs is the exported version of dumpLogs for use by tests.
func (sp *ServerProcess) DumpLogs(t *testing.T) {
	sp.dumpLogs(t)
}

// findDfsBinary locates the dfs binary, building it if necessary.
func findDfsBinary(t *testing.T) string {
	t.Helper()

	// Check for dfs in PATH
	if path, err := exec.LookPath("dfs"); err == nil {
		return path
	}

	// Check for dfs in project root
	projectRoot := findProjectRoot(t)
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

// findProjectRoot locates the project root by looking for go.mod.
func findProjectRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("Could not find project root (go.mod not found)")
		}
		dir = parent
	}
}

// StartServerProcessWithConfig starts a DittoFS server using a pre-created config file.
// Unlike StartServerProcess, this does NOT modify the config - it uses it as-is.
// The caller is responsible for setting appropriate ports, paths, etc. in the config.
// The caller should set DITTOFS_ADMIN_INITIAL_PASSWORD via t.Setenv before calling.
func StartServerProcessWithConfig(t *testing.T, configPath string) *ServerProcess {
	t.Helper()

	stateDir := t.TempDir()

	pidFile := filepath.Join(stateDir, "dfs.pid")
	logFile := filepath.Join(stateDir, "dfs.log")

	// Find the dfs binary
	dfsPath := findDfsBinary(t)

	// Start server in foreground mode
	cmd := exec.Command(dfsPath, "start", "--foreground",
		"--config", configPath,
		"--pid-file", pidFile,
		"--log-file", logFile)

	// Build environment for subprocess, keeping existing env vars
	// (caller should have set DITTOFS_ADMIN_INITIAL_PASSWORD via t.Setenv)
	cmd.Env = os.Environ()

	// Redirect stdout/stderr to log file
	logFileHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("Failed to create log file: %v", err)
	}

	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	// Start the process
	if err := cmd.Start(); err != nil {
		_ = logFileHandle.Close()
		t.Fatalf("Failed to start dfs server: %v", err)
	}

	// Extract API port from config by parsing it
	apiPort := extractAPIPortFromConfig(t, configPath)

	sp := &ServerProcess{
		cmd:           cmd,
		pidFile:       pidFile,
		apiPort:       apiPort,
		logFile:       logFile,
		stateDir:      stateDir,
		configFile:    configPath,
		process:       cmd.Process,
		logFileHandle: logFileHandle,
	}

	// Wait for server to be ready (longer timeout for Kerberos setup)
	if err := sp.WaitReady(15 * time.Second); err != nil {
		// Dump logs on failure
		sp.dumpLogs(t)
		sp.ForceKill()
		t.Fatalf("Server failed to become ready: %v", err)
	}

	return sp
}

// extractAPIPortFromConfig parses the config file to find the controlplane port.
func extractAPIPortFromConfig(t *testing.T, configPath string) int {
	t.Helper()

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	// Simple YAML parsing - look for "controlplane:" section and "port:" within it
	lines := strings.Split(string(content), "\n")
	inControlplane := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "controlplane:") {
			inControlplane = true
			continue
		}
		if inControlplane && strings.HasPrefix(trimmed, "port:") {
			var port int
			_, err := fmt.Sscanf(trimmed, "port: %d", &port)
			if err == nil && port > 0 {
				return port
			}
		}
		// Exit controlplane section if we hit another top-level key
		if inControlplane && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
	}

	t.Fatalf("Could not find controlplane port in config file: %s", configPath)
	return 0
}

// createConfigWithAPIPort creates a config file with the specified API port.
// If configPath is empty, creates a minimal default config.
func createConfigWithAPIPort(t *testing.T, configPath string, apiPort int, stateDir string) string {
	t.Helper()

	// Create minimal config for testing with in-memory stores
	// JWT secret must be at least 32 characters (under controlplane.jwt.secret)
	configContent := fmt.Sprintf(`# Test configuration generated by e2e test
logging:
  level: DEBUG
  format: text
  output: stdout

controlplane:
  port: %d
  jwt:
    secret: "test-secret-key-for-e2e-testing-only-must-be-32-chars"

database:
  type: sqlite
  sqlite:
    path: "%s/dittofs.db"

cache:
  path: "%s/cache"
  size: 104857600

metadata:
  stores:
    - name: default
      type: memory

payload:
  stores:
    - name: default
      type: memory

shares:
  - name: /export
    metadata_store: default
    payload_store: default
`, apiPort, stateDir, stateDir)

	configFile := filepath.Join(stateDir, "config.yaml")
	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	return configFile
}
