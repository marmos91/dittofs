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

// TestLoad_RequireInitialPasswordChange covers the opt-out knob for the forced
// admin first-login password change: it defaults to true, is settable to false
// via the config file, and is overridable to false via env.
func TestLoad_RequireInitialPasswordChange(t *testing.T) {
	t.Run("defaults to true when absent", func(t *testing.T) {
		content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
		cfg, err := Load(writeConfigFile(t, content))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.ControlPlane.RequiresInitialPasswordChange() {
			t.Error("expected forced password change on by default")
		}
	})

	t.Run("file opt-out", func(t *testing.T) {
		content := `
database:
  type: sqlite
controlplane:
  require_initial_password_change: false
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
		cfg, err := Load(writeConfigFile(t, content))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ControlPlane.RequiresInitialPasswordChange() {
			t.Error("expected forced password change off when file sets it false")
		}
	})

	t.Run("env opt-out", func(t *testing.T) {
		content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
		t.Setenv("DITTOFS_CONTROLPLANE_REQUIRE_INITIAL_PASSWORD_CHANGE", "false")
		cfg, err := Load(writeConfigFile(t, content))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ControlPlane.RequiresInitialPasswordChange() {
			t.Error("expected forced password change off when env sets it false")
		}
	})
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

// TestLoad_JWTSecretFromShortEnv verifies the documented short-form override
// DITTOFS_CONTROLPLANE_SECRET lands in cfg.ControlPlane.JWT.Secret when the
// key is omitted from the file. Before the fix this bound to controlplane.secret
// (a key no struct field reads), so the documented override was dead.
func TestLoad_JWTSecretFromShortEnv(t *testing.T) {
	content := `
database:
  type: sqlite
`
	path := writeConfigFile(t, content)

	const want = "short-form-secret-key-minimum-32-characters!"
	t.Setenv("DITTOFS_CONTROLPLANE_SECRET", want)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ControlPlane.JWT.Secret != want {
		t.Errorf("controlplane.jwt.secret: short-form env override dropped, got %q want %q", cfg.ControlPlane.JWT.Secret, want)
	}
}

// TestLoad_JWTSecretFromLongEnv verifies the auto long form
// DITTOFS_CONTROLPLANE_JWT_SECRET still resolves after the explicit BindEnv for
// the short form (a second BindEnv overwrites the env-var list, so both forms
// must be listed).
func TestLoad_JWTSecretFromLongEnv(t *testing.T) {
	content := `
database:
  type: sqlite
`
	path := writeConfigFile(t, content)

	const want = "long-form-secret-key-minimum-32-characters!"
	t.Setenv("DITTOFS_CONTROLPLANE_JWT_SECRET", want)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ControlPlane.JWT.Secret != want {
		t.Errorf("controlplane.jwt.secret: long-form env override dropped, got %q want %q", cfg.ControlPlane.JWT.Secret, want)
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
		"controlplane.host",
		"controlplane.port",
		"controlplane.tls.cert_file",
		"controlplane.tls.key_file",
		"controlplane.tls.client_ca",
		"controlplane.tls.min_version",
		"controlplane.jwt.secret",
		"controlplane.pprof",
		"controlplane.require_initial_password_change",
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

// TestLoad_EnvOnly_NoConfigFile verifies that env-var overrides are honoured
// even when NO config file exists on disk — the container/CI deployment case.
// Before the fix, Load short-circuited to GetDefaultConfig() on the no-file
// path, bypassing v.Unmarshal entirely and silently dropping every bound
// DITTOFS_* env var (violating the documented env > file > defaults precedence).
func TestLoad_EnvOnly_NoConfigFile(t *testing.T) {
	// Point at a path that does not exist so readConfigFile reports no file.
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	t.Setenv("DITTOFS_DATABASE_TYPE", "postgres")
	t.Setenv("DITTOFS_DATABASE_POSTGRES_HOST", "db.internal")
	t.Setenv("DITTOFS_DATABASE_POSTGRES_DATABASE", "dfs")
	t.Setenv("DITTOFS_DATABASE_POSTGRES_USER", "dfs")
	t.Setenv("DITTOFS_CONTROLPLANE_PORT", "9292")
	t.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR")
	t.Setenv("DITTOFS_CONTROLPLANE_SECRET", "env-only-secret-key-minimum-32-characters!")

	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database.Type != store.DatabaseTypePostgres {
		t.Errorf("database.type: env override dropped on no-file path, got %q want postgres", cfg.Database.Type)
	}
	if cfg.ControlPlane.Port != 9292 {
		t.Errorf("controlplane.port: env override dropped on no-file path, got %d want 9292", cfg.ControlPlane.Port)
	}
	if cfg.Logging.Level != "ERROR" {
		t.Errorf("logging.level: env override dropped on no-file path, got %q want ERROR", cfg.Logging.Level)
	}
	if cfg.ControlPlane.JWT.Secret != "env-only-secret-key-minimum-32-characters!" {
		t.Errorf("controlplane.jwt.secret: env override dropped on no-file path, got %q", cfg.ControlPlane.JWT.Secret)
	}
}

// TestLoad_NoFileNoEnv_StillDefaults guards that removing the GetDefaultConfig
// short-circuit did not regress the plain no-file/no-env path: defaults must
// still be applied for every field the env doesn't set.
func TestLoad_NoFileNoEnv_StillDefaults(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ControlPlane.Port != 8080 {
		t.Errorf("default controlplane.port: got %d want 8080", cfg.ControlPlane.Port)
	}
	if cfg.Admin.Username != "admin" {
		t.Errorf("default admin.username: got %q want admin", cfg.Admin.Username)
	}
	if cfg.Database.Type != store.DatabaseTypeSQLite {
		t.Errorf("default database.type: got %q want sqlite", cfg.Database.Type)
	}
}

// TestLoad_IdentityMachineSIDFromEnv verifies the machine-SID pin resolves from
// env (DITTOFS_IDENTITY_MACHINE_SID) and via the config file (AD-3 #1235).
func TestLoad_IdentityMachineSIDFromEnv(t *testing.T) {
	content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	t.Setenv("DITTOFS_IDENTITY_MACHINE_SID", "S-1-5-21-10-20-30")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Identity.MachineSID != "S-1-5-21-10-20-30" {
		t.Errorf("identity.machine_sid env override dropped, got %q", cfg.Identity.MachineSID)
	}
}

// TestLoad_IdentityMachineSIDInvalidRejected verifies validation rejects a
// malformed pinned machine SID.
func TestLoad_IdentityMachineSIDInvalidRejected(t *testing.T) {
	content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
identity:
  machine_sid: "not-a-sid"
`
	path := writeConfigFile(t, content)
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to reject invalid identity.machine_sid")
	}
}
