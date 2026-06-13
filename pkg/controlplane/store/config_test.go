package store

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestValidate_PostgresPoolSize(t *testing.T) {
	base := func() *Config {
		return &Config{
			Type: DatabaseTypePostgres,
			Postgres: PostgresConfig{
				Host:     "db.example.com",
				Database: "dittofs",
				User:     "dittofs",
			},
		}
	}

	t.Run("NegativeMaxOpenConnsRejected", func(t *testing.T) {
		cfg := base()
		cfg.Postgres.MaxOpenConns = -1
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected negative max_open_conns to be rejected (negative disables the connection cap)")
		}
	})

	t.Run("NegativeMaxIdleConnsRejected", func(t *testing.T) {
		cfg := base()
		cfg.Postgres.MaxIdleConns = -1
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected negative max_idle_conns to be rejected")
		}
	})

	t.Run("ZeroAndPositiveAccepted", func(t *testing.T) {
		cfg := base()
		cfg.Postgres.MaxOpenConns = 0 // 0 = use default
		cfg.Postgres.MaxIdleConns = 10
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected zero/positive pool sizes to validate, got %v", err)
		}
	})
}

// TestPostgresDSN_Encoding verifies that DSN() produces a valid, correctly
// percent-encoded URL even when credentials and paths contain special characters.
func TestPostgresDSN_Encoding(t *testing.T) {
	t.Run("simple credentials round-trip", func(t *testing.T) {
		cfg := PostgresConfig{
			Host:     "db.example.com",
			Port:     5432,
			Database: "dittofs",
			User:     "admin",
			Password: "secret",
			SSLMode:  "require",
		}
		dsn := cfg.DSN()
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("DSN() produced an unparseable URL %q: %v", dsn, err)
		}
		if u.Scheme != "postgres" {
			t.Errorf("scheme = %q, want postgres", u.Scheme)
		}
		if u.Hostname() != "db.example.com" {
			t.Errorf("host = %q, want db.example.com", u.Hostname())
		}
		if u.Port() != "5432" {
			t.Errorf("port = %q, want 5432", u.Port())
		}
		if strings.TrimPrefix(u.Path, "/") != "dittofs" {
			t.Errorf("database = %q, want dittofs", u.Path)
		}
		user, pass, ok := func() (string, string, bool) {
			if u.User == nil {
				return "", "", false
			}
			p, set := u.User.Password()
			return u.User.Username(), p, set
		}()
		if !ok || user != "admin" || pass != "secret" {
			t.Errorf("credentials = (%q, %q, ok=%v), want (admin, secret, true)", user, pass, ok)
		}
		if u.Query().Get("sslmode") != "require" {
			t.Errorf("sslmode = %q, want require", u.Query().Get("sslmode"))
		}
	})

	t.Run("password with spaces is percent-encoded and does not corrupt DSN", func(t *testing.T) {
		cfg := PostgresConfig{
			Host:     "localhost",
			Port:     5432,
			Database: "mydb",
			User:     "service account", // space in username
			Password: "p@ss word!#1",    // space, @, !, # in password
			SSLMode:  "disable",
		}
		dsn := cfg.DSN()

		// Before the fix this produced:
		//   host=localhost port=5432 user=service account password=p@ss word!#1 dbname=mydb
		// which libpq mis-parses (space terminates keyword=value tokens).
		// After the fix the DSN must be a valid URL with no raw spaces.
		if strings.Contains(dsn, " ") {
			t.Errorf("DSN() contains a raw space — libpq will mis-parse it: %q", dsn)
		}

		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("DSN() produced an unparseable URL %q: %v", dsn, err)
		}
		gotUser := u.User.Username()
		gotPass, _ := u.User.Password()
		if gotUser != "service account" {
			t.Errorf("decoded user = %q, want %q", gotUser, "service account")
		}
		if gotPass != "p@ss word!#1" {
			t.Errorf("decoded password = %q, want %q", gotPass, "p@ss word!#1")
		}
	})

	t.Run("password with single-quote and backslash (libpq quoting characters)", func(t *testing.T) {
		cfg := PostgresConfig{
			Host:     "localhost",
			Port:     5432,
			Database: "db",
			User:     "u",
			Password: `it's a\tricky'pass`,
			SSLMode:  "disable",
		}
		dsn := cfg.DSN()

		// Raw single-quote and backslash would break libpq keyword=value quoting.
		// In URL form net/url percent-encodes them safely.
		if strings.ContainsAny(dsn, `'`) {
			t.Errorf("DSN() contains a raw single-quote — would break libpq keyword=value parsing: %q", dsn)
		}

		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("DSN() produced an unparseable URL %q: %v", dsn, err)
		}
		gotPass, _ := u.User.Password()
		if gotPass != `it's a\tricky'pass` {
			t.Errorf("decoded password = %q, want %q", gotPass, `it's a\tricky'pass`)
		}
	})

	t.Run("SSLRootCert with space in path is encoded in query string", func(t *testing.T) {
		cfg := PostgresConfig{
			Host:        "localhost",
			Port:        5432,
			Database:    "db",
			User:        "u",
			Password:    "pass",
			SSLMode:     "verify-full",
			SSLRootCert: "/path/to/my certs/root.crt",
		}
		dsn := cfg.DSN()
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("DSN() produced an unparseable URL %q: %v", dsn, err)
		}
		cert := u.Query().Get("sslrootcert")
		if cert != "/path/to/my certs/root.crt" {
			t.Errorf("sslrootcert = %q, want %q", cert, "/path/to/my certs/root.crt")
		}
	})

	t.Run("empty SSLMode defaults to disable", func(t *testing.T) {
		cfg := PostgresConfig{
			Host:     "localhost",
			Port:     5432,
			Database: "db",
			User:     "u",
			Password: "pass",
		}
		dsn := cfg.DSN()
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("unparseable DSN: %v", err)
		}
		if got := u.Query().Get("sslmode"); got != "disable" {
			t.Errorf("sslmode = %q, want disable", got)
		}
	})
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
