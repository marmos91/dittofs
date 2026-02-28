package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestApplyDefaults_SQLitePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Run("UsesAPPDATA", func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("APPDATA", tmpDir)

			cfg := &Config{Type: DatabaseTypeSQLite}
			cfg.ApplyDefaults()

			expected := filepath.Join(tmpDir, "dittofs", "controlplane.db")
			if cfg.SQLite.Path != expected {
				t.Errorf("SQLite.Path = %q, expected %q", cfg.SQLite.Path, expected)
			}
		})

		t.Run("FallbackWithoutAPPDATA", func(t *testing.T) {
			t.Setenv("APPDATA", "")

			cfg := &Config{Type: DatabaseTypeSQLite}
			cfg.ApplyDefaults()

			// Should contain AppData/Roaming/dittofs/controlplane.db
			if filepath.Base(cfg.SQLite.Path) != "controlplane.db" {
				t.Errorf("SQLite.Path = %q, expected filename 'controlplane.db'", cfg.SQLite.Path)
			}
		})
	} else {
		t.Run("UsesXDGConfigHome", func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmpDir)

			cfg := &Config{Type: DatabaseTypeSQLite}
			cfg.ApplyDefaults()

			expected := filepath.Join(tmpDir, "dittofs", "controlplane.db")
			if cfg.SQLite.Path != expected {
				t.Errorf("SQLite.Path = %q, expected %q", cfg.SQLite.Path, expected)
			}
		})

		t.Run("FallbackWithoutXDG", func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "")

			cfg := &Config{Type: DatabaseTypeSQLite}
			cfg.ApplyDefaults()

			// Should end with .config/dittofs/controlplane.db
			if filepath.Base(cfg.SQLite.Path) != "controlplane.db" {
				t.Errorf("SQLite.Path = %q, expected filename 'controlplane.db'", cfg.SQLite.Path)
			}
			dir := filepath.Dir(cfg.SQLite.Path)
			if filepath.Base(dir) != "dittofs" {
				t.Errorf("parent dir = %q, expected 'dittofs'", filepath.Base(dir))
			}
			home, _ := os.UserHomeDir()
			expectedDir := filepath.Join(home, ".config", "dittofs")
			if dir != expectedDir {
				t.Errorf("dir = %q, expected %q", dir, expectedDir)
			}
		})
	}
}

func TestApplyDefaults_PreservesExplicitPath(t *testing.T) {
	customPath := "/custom/path/to/db.sqlite"
	cfg := &Config{
		Type:   DatabaseTypeSQLite,
		SQLite: SQLiteConfig{Path: customPath},
	}
	cfg.ApplyDefaults()

	if cfg.SQLite.Path != customPath {
		t.Errorf("SQLite.Path = %q, expected %q (explicit path should be preserved)", cfg.SQLite.Path, customPath)
	}
}
