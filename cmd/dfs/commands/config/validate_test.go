package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// minimalConfig is a loadable config body; the JWT secret is templated in so a
// test can exercise both the warning (empty secret) and clean (set secret)
// validate paths.
func minimalConfig(jwtSecret string) string {
	return `logging:
  level: "INFO"
  format: "text"
  output: "stdout"
shutdown_timeout: 30s
database:
  type: sqlite
  sqlite:
    path: ""
controlplane:
  port: 8080
  jwt:
    secret: "` + jwtSecret + `"
`
}

// runValidateCapture invokes runConfigValidate against the given config file
// path, capturing stdout. It mirrors the persistent --config string flag the
// root dfs command registers.
func runValidateCapture(t *testing.T, configPath string) (string, error) {
	t.Helper()

	cmd := &cobra.Command{}
	cmd.Flags().String("config", "", "")
	if err := cmd.Flags().Set("config", configPath); err != nil {
		t.Fatalf("set --config flag: %v", err)
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	runErr := runConfigValidate(cmd, nil)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String(), runErr
}

// TestConfigValidate_WarningsAnnotateOK asserts that a config which loads but
// carries a warning (no JWT secret -> "API authentication will fail") does not
// print a bare "Validation: OK"; the warning count is surfaced so an operator
// eyeballing the output is not misled into believing the server can serve
// authenticated requests.
func TestConfigValidate_WarningsAnnotateOK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig("")), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, err := runValidateCapture(t, path)
	if err != nil {
		t.Fatalf("runConfigValidate returned error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Validation: OK (with") {
		t.Errorf("expected annotated OK with warning count, got:\n%s", out)
	}
	if strings.Contains(out, "Validation: OK\n") {
		t.Errorf("bare 'Validation: OK' must not be printed when warnings are present, got:\n%s", out)
	}
	if !strings.Contains(out, "JWT secret not configured") {
		t.Errorf("expected the JWT-secret warning to be listed, got:\n%s", out)
	}
}

// TestConfigValidate_NoWarningsBareOK asserts the clean path still prints a
// bare "Validation: OK" with no warnings section.
func TestConfigValidate_NoWarningsBareOK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	secret := strings.Repeat("a", 48) // >= 32 chars
	if err := os.WriteFile(path, []byte(minimalConfig(secret)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, err := runValidateCapture(t, path)
	if err != nil {
		t.Fatalf("runConfigValidate returned error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Validation: OK") {
		t.Errorf("expected 'Validation: OK', got:\n%s", out)
	}
	if strings.Contains(out, "(with") {
		t.Errorf("clean config must not annotate a warning count, got:\n%s", out)
	}
	if strings.Contains(out, "Warnings:") {
		t.Errorf("clean config must not print a Warnings section, got:\n%s", out)
	}
}
