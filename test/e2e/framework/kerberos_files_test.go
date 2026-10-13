//go:build e2e

package framework

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// These run installFiles on paths in a temp dir, so they need no root. Each
// installs inside a subtest, whose cleanups have run by the time t.Run
// returns, and then checks what the path holds afterwards.

func writeFile(t *testing.T, path, content string, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestInstallFiles_RemovesAFileTheHostDidNotHave(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "test.keytab")
	dst := filepath.Join(dir, "krb5.keytab")
	writeFile(t, src, "test realm keys", 0600)

	t.Run("install", func(t *testing.T) {
		if err := installFiles(t, []installedFile{{path: dst, src: src, perm: 0600}}); err != nil {
			t.Fatalf("installFiles: %v", err)
		}
		if got := readFile(t, dst); got != "test realm keys" {
			t.Fatalf("installed content = %q, want the test file's", got)
		}
	})

	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the test, %s should not exist (the host had none), stat err = %v", dst, err)
	}
}

func TestInstallFiles_RestoresTheHostsFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "test.conf")
	dst := filepath.Join(dir, "krb5.conf")
	writeFile(t, src, "test realm", 0644)
	writeFile(t, dst, "host realm", 0644)

	t.Run("install", func(t *testing.T) {
		if err := installFiles(t, []installedFile{{path: dst, src: src, perm: 0644}}); err != nil {
			t.Fatalf("installFiles: %v", err)
		}
		if got := readFile(t, dst); got != "test realm" {
			t.Fatalf("installed content = %q, want the test file's", got)
		}
	})

	if got := readFile(t, dst); got != "host realm" {
		t.Fatalf("after the test, %s = %q, want the host's content back", dst, got)
	}
}

// The restore is registered before anything is written, so when the second
// file can't be installed, the first one is still put back.
func TestInstallFiles_UndoesAPartialInstall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "test.conf")
	conf := filepath.Join(dir, "krb5.conf")
	keytab := filepath.Join(dir, "krb5.keytab")
	writeFile(t, src, "test realm", 0644)

	t.Run("install", func(t *testing.T) {
		err := installFiles(t, []installedFile{
			{path: conf, src: src, perm: 0644},
			{path: keytab, src: filepath.Join(dir, "missing.keytab"), perm: 0600},
		})
		if err == nil {
			t.Fatal("installFiles succeeded with a missing source")
		}
	})

	for _, p := range []string{conf, keytab} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("after a failed install, %s should not exist, stat err = %v", p, err)
		}
	}
}

// A host file that can't be read couldn't be restored, so it is not replaced.
func TestInstallFiles_LeavesAnUnreadableFileAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file, so there is no unreadable file to test with")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "test.keytab")
	dst := filepath.Join(dir, "krb5.keytab")
	writeFile(t, src, "test realm keys", 0600)
	writeFile(t, dst, "host keys", 0000)
	t.Cleanup(func() { _ = os.Chmod(dst, 0600) })

	t.Run("install", func(t *testing.T) {
		if err := installFiles(t, []installedFile{{path: dst, src: src, perm: 0600}}); err == nil {
			t.Fatal("installFiles replaced a file it could not read")
		}
	})

	if err := os.Chmod(dst, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if got := readFile(t, dst); got != "host keys" {
		t.Fatalf("after the test, %s = %q, want it untouched", dst, got)
	}
}
