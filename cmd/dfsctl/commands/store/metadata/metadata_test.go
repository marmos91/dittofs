package metadata

import (
	"testing"
)

// TestMetadataCmd_RegistersExistingVerbs guards against regressions where
// subcommand wiring accidentally displaces pre-existing verbs.
func TestMetadataCmd_RegistersExistingVerbs(t *testing.T) {
	required := []string{"list", "add", "edit", "remove", "health"}
	for _, want := range required {
		if _, _, err := Cmd.Find([]string{want}); err != nil {
			t.Errorf("existing verb %q lost from metadata.Cmd: %v", want, err)
		}
	}
}

// TestBuildMetadataConfig pins the CLI's accepted store type to the server's
// (pkg/controlplane/runtime.CreateMetadataStoreFromConfig): badger only.
func TestBuildMetadataConfig(t *testing.T) {
	if _, err := buildMetadataConfig("badger", "", t.TempDir(), false); err != nil {
		t.Errorf("badger with a path rejected: %v", err)
	}
	cfg, err := buildMetadataConfig("badger", "", "", true)
	if err != nil {
		t.Fatalf("badger in memory rejected: %v", err)
	}
	if m, _ := cfg.(map[string]any); m["in_memory"] != true {
		t.Errorf("--in-memory config = %v, want in_memory: true", cfg)
	}
	for _, removed := range []string{"memory", "sqlite", "postgres", "nonesuch"} {
		if _, err := buildMetadataConfig(removed, "", "", false); err == nil {
			t.Errorf("type %q should be rejected, got nil error", removed)
		}
	}
}
