package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// writeConfigFile writes content to a temp config.yaml and returns its path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoad_EnvOverride_KeyAbsentFromFile verifies that an env var overrides a
// key that is NOT present in the config file — the round-1 §2.2 / round-2 §2.4
// env-precedence-drop class. The documented precedence is env > file >
// defaults, which only holds if every key is bound for AutomaticEnv.
func TestLoad_EnvOverride_KeyAbsentFromFile(t *testing.T) {
	// File omits controlplane.port and logging.level entirely.
	content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	t.Setenv("DITTOFS_CONTROLPLANE_PORT", "9191")
	t.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ControlPlane.Port != 9191 {
		t.Errorf("controlplane.port: env override dropped, got %d want 9191", cfg.ControlPlane.Port)
	}
	if cfg.Logging.Level != "ERROR" {
		t.Errorf("logging.level: env override dropped, got %q want ERROR", cfg.Logging.Level)
	}
}

// TestLoad_DatabaseTypeFromEnv_TypeOmittedFromFile reproduces round-2 §2.2(b):
// a container supplies postgres connection details in the file but omits
// database.type, then sets DITTOFS_DATABASE_TYPE=postgres via env. Before the
// fix the env var was silently dropped and the server booted on SQLite.
func TestLoad_DatabaseTypeFromEnv_TypeOmittedFromFile(t *testing.T) {
	// type is intentionally omitted from the file.
	content := `
database:
  postgres:
    host: db.internal
    database: dfs
    user: dfs
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	t.Setenv("DITTOFS_DATABASE_TYPE", "postgres")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database.Type != store.DatabaseTypePostgres {
		t.Fatalf("database.type: env override dropped, got %q want postgres", cfg.Database.Type)
	}
}

// TestLoad_PostgresDocumentedKeys verifies the EXACT multi-word postgres keys
// documented in docs/CONFIGURATION.md parse into the struct from a YAML file —
// round-2 §2.2(a). Before the struct tags were added these silently decoded to
// zero values (and ssl_root_cert being unsettable was a silent TLS downgrade).
func TestLoad_PostgresDocumentedKeys(t *testing.T) {
	content := `
database:
  type: postgres
  postgres:
    host: db.internal
    port: 5433
    database: dfs
    user: dfs
    password: filesecret
    sslmode: verify-full
    ssl_root_cert: /etc/ca.pem
    max_open_conns: 99
    max_idle_conns: 7
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	pg := cfg.Database.Postgres
	if pg.SSLRootCert != "/etc/ca.pem" {
		t.Errorf("ssl_root_cert: got %q want /etc/ca.pem", pg.SSLRootCert)
	}
	if pg.SSLMode != "verify-full" {
		t.Errorf("sslmode: got %q want verify-full", pg.SSLMode)
	}
	if pg.MaxOpenConns != 99 {
		t.Errorf("max_open_conns: got %d want 99", pg.MaxOpenConns)
	}
	if pg.MaxIdleConns != 7 {
		t.Errorf("max_idle_conns: got %d want 7", pg.MaxIdleConns)
	}
}

// TestLoad_PostgresKeysFromEnv verifies the documented postgres keys can also
// be set via env (round-2 §2.2(a) env dimension), now that every key is bound.
func TestLoad_PostgresKeysFromEnv(t *testing.T) {
	content := `
database:
  type: postgres
  postgres:
    host: db.internal
    database: dfs
    user: dfs
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	t.Setenv("DITTOFS_DATABASE_POSTGRES_SSL_ROOT_CERT", "/etc/env-ca.pem")
	t.Setenv("DITTOFS_DATABASE_POSTGRES_MAX_OPEN_CONNS", "42")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database.Postgres.SSLRootCert != "/etc/env-ca.pem" {
		t.Errorf("ssl_root_cert from env: got %q want /etc/env-ca.pem", cfg.Database.Postgres.SSLRootCert)
	}
	if cfg.Database.Postgres.MaxOpenConns != 42 {
		t.Errorf("max_open_conns from env: got %d want 42", cfg.Database.Postgres.MaxOpenConns)
	}
}

// TestConfigEnvKeys_CoversKnownKeys asserts the reflective key walk produces
// the load-bearing nested key paths. Guards against a refactor that drops a
// nested namespace from the BindEnv walk (which would silently reintroduce the
// env-drop bug).
func TestConfigEnvKeys_CoversKnownKeys(t *testing.T) {
	keys := configEnvKeys()
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}

	want := []string{
		"logging.level",
		"logging.rotation.max_size",
		"shutdown_timeout",
		"database.type",
		"database.sqlite.path",
		"database.postgres.host",
		"database.postgres.ssl_root_cert",
		"database.postgres.max_open_conns",
		"database.postgres.max_idle_conns",
		"controlplane.port",
		"controlplane.jwt.secret",
		"controlplane.pprof",
		"admin.username",
		"kerberos.enabled",
		"gc.grace_period",
		"snapshot.restore_http_timeout",
	}
	for _, k := range want {
		if !set[k] {
			t.Errorf("configEnvKeys() missing %q (got %v)", k, keys)
		}
	}
}
