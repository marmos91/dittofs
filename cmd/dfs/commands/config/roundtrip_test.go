package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/marmos91/dittofs/internal/cli/output"
	dfsconfig "github.com/marmos91/dittofs/pkg/config"
)

// expectedTopLevelYAMLKeys mirrors the yaml/mapstructure tags on the top-level
// dfsconfig.Config fields. If a field is added/removed there this test fails
// loudly, which is the point: the schema must track the real YAML namespace.
var expectedTopLevelYAMLKeys = []string{
	"logging",
	"shutdown_timeout",
	"database",
	"controlplane",
	"admin",
	"kerberos",
	"blockstore",
	"gc",
	"snapshot",
}

// TestConfigSchema_TopLevelKeysAreYAMLKeys asserts the generated JSON schema's
// top-level property keys equal the lowercase yaml keys (not Go PascalCase
// field names), so an IDE/validator using the schema accepts a real
// config.yaml.
func TestConfigSchema_TopLevelKeysAreYAMLKeys(t *testing.T) {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
		FieldNameTag:              "yaml",
	}
	schema := reflector.Reflect(&dfsconfig.Config{})

	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var decoded struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	for _, key := range expectedTopLevelYAMLKeys {
		if _, ok := decoded.Properties[key]; !ok {
			t.Errorf("schema missing expected lowercase yaml key %q; got keys %v", key, keysOf(decoded.Properties))
		}
	}
	for got := range decoded.Properties {
		// PascalCase Go field names must not leak into the schema.
		if got != "" && got[0] >= 'A' && got[0] <= 'Z' {
			t.Errorf("schema emitted PascalCase key %q (should be the lowercase yaml key)", got)
		}
	}
}

// TestConfigShowJSON_RoundTripsThroughLoad asserts that the JSON emitted by
// `config show -o json` re-parses through config.Load.
func TestConfigShowJSON_RoundTripsThroughLoad(t *testing.T) {
	cfg := dfsconfig.GetDefaultConfig()

	keyed, err := yamlKeyedView(cfg)
	if err != nil {
		t.Fatalf("yamlKeyedView: %v", err)
	}

	var buf bytes.Buffer
	if err := output.PrintJSON(&buf, keyed); err != nil {
		t.Fatalf("PrintJSON: %v", err)
	}

	// config.Load expects a file; viper auto-detects YAML/JSON by content.
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(jsonPath, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write json: %v", err)
	}

	reloaded, err := dfsconfig.Load(jsonPath)
	if err != nil {
		t.Fatalf("Load could not re-parse `config show -o json` output: %v\noutput was:\n%s", err, buf.String())
	}

	if reloaded.Logging.Level != cfg.Logging.Level {
		t.Errorf("round-trip logging.level mismatch: got %q want %q", reloaded.Logging.Level, cfg.Logging.Level)
	}
	if reloaded.ShutdownTimeout != cfg.ShutdownTimeout {
		t.Errorf("round-trip shutdown_timeout mismatch: got %v want %v", reloaded.ShutdownTimeout, cfg.ShutdownTimeout)
	}
	if reloaded.Database.Type != cfg.Database.Type {
		t.Errorf("round-trip database.type mismatch: got %q want %q", reloaded.Database.Type, cfg.Database.Type)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
