package commands

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionCmd_OutputPrefix verifies that "dfs version" prints the binary
// name returned by cmd.Root().Name() — not the hardcoded string "dittofs".
func TestVersionCmd_OutputPrefix(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	t.Cleanup(func() { rootCmd.SetOut(nil) })

	rootCmd.SetArgs([]string{"version"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, "dfs ") {
		t.Errorf("version output should start with %q, got: %q", "dfs ", out)
	}
	if strings.HasPrefix(out, "dittofs ") {
		t.Errorf("version output must not start with hardcoded %q, got: %q", "dittofs ", out)
	}
}

// TestVersionCmd_ShortFlag verifies --short prints only the version token with
// no binary-name prefix (unchanged behaviour).
func TestVersionCmd_ShortFlag(t *testing.T) {
	Version = "v1.2.3"
	t.Cleanup(func() { Version = "dev" })

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	t.Cleanup(func() { rootCmd.SetOut(nil) })

	rootCmd.SetArgs([]string{"version", "--short"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := strings.TrimSpace(buf.String())
	if out != "v1.2.3" {
		t.Errorf("--short output = %q, want %q", out, "v1.2.3")
	}
}
