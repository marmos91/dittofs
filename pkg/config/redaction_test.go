package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"gopkg.in/yaml.v3"
)

// TestConfigShow_RedactsSecrets asserts that serializing the live *Config for
// display (the `dfs config show` path, json + yaml) never emits the JWT
// signing secret, the postgres password, or the admin password hash —
// round-2 §2.3 / round-1 §2.1.
func TestConfigShow_RedactsSecrets(t *testing.T) {
	const (
		jwtSecret   = "super-secret-jwt-signing-key-32-chars-long"
		pgPass      = "p0stgr3s-pa55word"
		adminHash   = "$2y$10$abcdefghijklmnopqrstuv"
		ldapBindPwd = "ldap-b1nd-s3cret"
	)

	cfg := GetDefaultConfig()
	cfg.ControlPlane.JWT.Secret = jwtSecret
	cfg.Database.Type = store.DatabaseTypePostgres
	cfg.Database.Postgres = store.PostgresConfig{
		Host:     "db",
		Database: "dfs",
		User:     "dfs",
		Password: pgPass,
	}
	cfg.Admin.PasswordHash = adminHash
	cfg.LDAP.BindPassword = ldapBindPwd

	secrets := []string{jwtSecret, pgPass, adminHash, ldapBindPwd}

	t.Run("yaml", func(t *testing.T) {
		out, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatalf("yaml.Marshal: %v", err)
		}
		assertNoSecrets(t, string(out), secrets)
	})

	t.Run("json", func(t *testing.T) {
		out, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		assertNoSecrets(t, string(out), secrets)
	})
}

func assertNoSecrets(t *testing.T, out string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("serialized config leaked secret %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "********") {
		t.Errorf("expected redaction sentinel in output:\n%s", out)
	}
}

// TestJWTConfig_EmptySecretNotRedacted ensures an unset secret stays empty
// (distinguishable from a redacted one) rather than being masked.
func TestJWTConfig_EmptySecretNotRedacted(t *testing.T) {
	c := api.JWTConfig{}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "********") {
		t.Errorf("empty secret should not be redacted: %s", out)
	}
}
