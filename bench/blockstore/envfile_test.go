package blockstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const body = `# comment
BENCH_TEST_KEY1=value1
BENCH_TEST_KEY2="quoted value"
BENCH_TEST_KEY3='single quoted'
export BENCH_TEST_KEY4=with-export

BENCH_TEST_KEY5= trailing-space-stripped
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	for _, k := range []string{"BENCH_TEST_KEY1", "BENCH_TEST_KEY2", "BENCH_TEST_KEY3", "BENCH_TEST_KEY4", "BENCH_TEST_KEY5"} {
		t.Setenv(k, "")
		// Clear via Unsetenv since t.Setenv("",  "") still records a value.
		_ = os.Unsetenv(k)
	}
	if err := ParseEnvFile(path); err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	want := map[string]string{
		"BENCH_TEST_KEY1": "value1",
		"BENCH_TEST_KEY2": "quoted value",
		"BENCH_TEST_KEY3": "single quoted",
		"BENCH_TEST_KEY4": "with-export",
		"BENCH_TEST_KEY5": "trailing-space-stripped",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
		_ = os.Unsetenv(k)
	}
}

func TestParseEnvFile_DoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("BENCH_PRESET=fromfile\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("BENCH_PRESET", "fromenv")
	if err := ParseEnvFile(path); err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if got := os.Getenv("BENCH_PRESET"); got != "fromenv" {
		t.Errorf("BENCH_PRESET = %q, want preserved %q", got, "fromenv")
	}
}

func TestParseEnvFile_MissingPathIsNoOp(t *testing.T) {
	if err := ParseEnvFile(""); err != nil {
		t.Errorf("empty path: %v", err)
	}
	if err := ParseEnvFile(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}
