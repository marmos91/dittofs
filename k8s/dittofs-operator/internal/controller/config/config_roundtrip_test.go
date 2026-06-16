package config

import (
	"sort"
	"strings"
	"testing"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

// serverKnownKeys is the pinned set of dotted config keys that the DittoFS
// server's pkg/config schema actually consumes for the infrastructure-only
// config the operator renders. Because the operator lives in a separate Go
// module, we cannot import the server's config struct directly, so this set is
// pinned here as the canonical contract.
//
// Sources (server tree, develop):
//   - pkg/config/config.go: Config{Logging, ShutdownTimeout, Database,
//     ControlPlane, Admin, ...}
//   - pkg/controlplane/api/config.go: APIConfig{Port, ..., JWT} and JWTConfig
//     {Secret, AccessTokenDuration, RefreshTokenDuration} — NOTE: no Issuer
//     field; the issuer is hardcoded "dittofs" at api/server.go.
//   - store.Config: Database{Type, SQLite{Path}, Postgres{...}}.
//
// If a future operator change emits a key not in this set, the round-trip test
// fails — catching operator->server config drift (the class that hid the dead
// jwt.issuer key).
var serverKnownKeys = map[string]bool{
	"logging.level":  true,
	"logging.format": true,
	"logging.output": true,

	"shutdown_timeout": true,

	"database.type":          true,
	"database.sqlite.path":   true,
	"database.postgres.host": true,
	// Postgres placeholder fields the operator emits so viper registers the
	// keys for env-var override; all map to store.Config Postgres fields.
	"database.postgres.port":     true,
	"database.postgres.database": true,
	"database.postgres.user":     true,
	"database.postgres.password": true,
	"database.postgres.sslmode":  true,

	"controlplane.host":                       true,
	"controlplane.port":                       true,
	"controlplane.jwt.access_token_duration":  true,
	"controlplane.jwt.refresh_token_duration": true,
	// Native TLS keys consumed by pkg/controlplane/api TLSConfig.
	"controlplane.tls.cert_file": true,
	"controlplane.tls.key_file":  true,
	"controlplane.tls.client_ca": true,

	"admin.username": true,
	"admin.email":    true,

	// Metrics keys consumed by pkg/config MetricsConfig (server tree, develop):
	//   MetricsConfig{Enabled, Host, Port, Path, Auth, TokenFile, TLS}.
	// The operator emits the scalar subset (no metrics.tls.* — in-cluster the
	// endpoint is plain HTTP behind the metrics Service + NetworkPolicy).
	"metrics.enabled":    true,
	"metrics.host":       true,
	"metrics.port":       true,
	"metrics.path":       true,
	"metrics.auth":       true,
	"metrics.token_file": true,

	// The server has no Cache field; the rendered cache.* block is a known
	// dead key tracked separately (round-1 M-CFG-1). It is intentionally NOT
	// listed here so that, were the cache block to grow new keys, this test
	// would flag it — but to avoid a spurious failure on the already-known
	// dead block we allowlist exactly the two keys the operator emits today.
	"cache.path": true,
	"cache.size": true,
}

// deadKeys are keys the operator must never emit because the server silently
// discards them (no corresponding pkg/config field). jwt.issuer was dropped in
// this change; this guards against its reintroduction.
var deadKeys = []string{
	"controlplane.jwt.issuer",
}

// flattenYAML walks a decoded YAML document and returns the set of dotted leaf
// key paths (scalar-valued keys).
func flattenYAML(t *testing.T, m map[string]any, prefix string, out map[string]bool) {
	t.Helper()
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch child := v.(type) {
		case map[string]any:
			flattenYAML(t, child, key, out)
		default:
			out[key] = true
		}
	}
}

func renderKeys(t *testing.T, ds *dittoiov1alpha1.DittoServer) map[string]bool {
	t.Helper()
	yamlStr, err := GenerateDittoFSConfig(ds)
	if err != nil {
		t.Fatalf("GenerateDittoFSConfig: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		t.Fatalf("unmarshal rendered config: %v\n%s", err, yamlStr)
	}
	out := map[string]bool{}
	flattenYAML(t, doc, "", out)
	return out
}

func TestGenerateDittoFSConfig_NoDeadKeys(t *testing.T) {
	cases := map[string]*dittoiov1alpha1.DittoServer{
		"minimal": {},
		"postgres": {
			Spec: dittoiov1alpha1.DittoServerSpec{
				Database: &dittoiov1alpha1.DatabaseConfig{
					Type:              "postgres",
					PostgresSecretRef: &corev1.SecretKeySelector{},
				},
			},
		},
		"full-jwt": {
			Spec: dittoiov1alpha1.DittoServerSpec{
				Identity: &dittoiov1alpha1.IdentityConfig{
					JWT: &dittoiov1alpha1.JWTConfig{
						AccessTokenDuration:  "30m",
						RefreshTokenDuration: "200h",
					},
					Admin: &dittoiov1alpha1.AdminConfig{Username: "root"},
				},
			},
		},
		"native-mtls": {
			Spec: dittoiov1alpha1.DittoServerSpec{
				ControlPlane: &dittoiov1alpha1.ControlPlaneAPIConfig{
					TLS:                true,
					CertSecretName:     "tls",
					ClientCASecretName: "ca",
				},
			},
		},
		"metrics-token": {
			Spec: dittoiov1alpha1.DittoServerSpec{
				Metrics: &dittoiov1alpha1.MetricsSpec{
					Enabled:           true,
					BearerTokenSecret: &corev1.SecretKeySelector{},
				},
			},
		},
	}

	for name, ds := range cases {
		t.Run(name, func(t *testing.T) {
			keys := renderKeys(t, ds)

			// 1. No dead key is emitted.
			for _, dk := range deadKeys {
				if keys[dk] {
					t.Errorf("operator emitted dead config key %q the server silently discards", dk)
				}
			}

			// 2. Every emitted key corresponds to a real server key.
			var unknown []string
			for k := range keys {
				if !serverKnownKeys[k] {
					unknown = append(unknown, k)
				}
			}
			sort.Strings(unknown)
			if len(unknown) > 0 {
				t.Errorf("operator emitted config keys with no matching server pkg/config field (drift): %s",
					strings.Join(unknown, ", "))
			}
		})
	}
}

// TestGenerateDittoFSConfig_IssuerNotRendered is a focused regression guard for
// the M-CFG-ISSUER fix: the rendered YAML must not contain an issuer key.
func TestGenerateDittoFSConfig_IssuerNotRendered(t *testing.T) {
	yamlStr, err := GenerateDittoFSConfig(&dittoiov1alpha1.DittoServer{})
	if err != nil {
		t.Fatalf("GenerateDittoFSConfig: %v", err)
	}
	if strings.Contains(yamlStr, "issuer") {
		t.Errorf("rendered config still references the dead jwt issuer key:\n%s", yamlStr)
	}
}

// TestGenerateDittoFSConfig_MetricsOmittedWhenDisabled verifies the metrics:
// block is absent unless explicitly enabled, preserving the server's
// disabled-by-default behavior.
func TestGenerateDittoFSConfig_MetricsOmittedWhenDisabled(t *testing.T) {
	yamlStr, err := GenerateDittoFSConfig(&dittoiov1alpha1.DittoServer{})
	if err != nil {
		t.Fatalf("GenerateDittoFSConfig: %v", err)
	}
	if strings.Contains(yamlStr, "metrics:") {
		t.Errorf("rendered config should not contain a metrics block when disabled:\n%s", yamlStr)
	}
}

// TestGenerateDittoFSConfig_MetricsEnabled verifies the rendered metrics block
// binds 0.0.0.0 and, with a bearer token Secret, switches auth to token with
// the mounted token-file path.
func TestGenerateDittoFSConfig_MetricsEnabled(t *testing.T) {
	ds := &dittoiov1alpha1.DittoServer{
		Spec: dittoiov1alpha1.DittoServerSpec{
			Metrics: &dittoiov1alpha1.MetricsSpec{
				Enabled:           true,
				Port:              19090,
				Path:              "/m",
				BearerTokenSecret: &corev1.SecretKeySelector{},
			},
		},
	}
	yamlStr, err := GenerateDittoFSConfig(ds)
	if err != nil {
		t.Fatalf("GenerateDittoFSConfig: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := doc["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("metrics block missing:\n%s", yamlStr)
	}
	if m["enabled"] != true {
		t.Errorf("metrics.enabled = %v, want true", m["enabled"])
	}
	if m["host"] != "0.0.0.0" {
		t.Errorf("metrics.host = %v, want 0.0.0.0", m["host"])
	}
	if m["port"] != 19090 {
		t.Errorf("metrics.port = %v, want 19090", m["port"])
	}
	if m["path"] != "/m" {
		t.Errorf("metrics.path = %v, want /m", m["path"])
	}
	if m["auth"] != "token" {
		t.Errorf("metrics.auth = %v, want token", m["auth"])
	}
	if m["token_file"] != dittoiov1alpha1.MetricsTokenFilePath() {
		t.Errorf("metrics.token_file = %v, want %s", m["token_file"], dittoiov1alpha1.MetricsTokenFilePath())
	}
}
