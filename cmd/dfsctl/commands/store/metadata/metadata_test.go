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

// TestBuildMetadataConfig_AcceptsServerSupportedTypes pins the CLI's accepted
// store types to the server's. The switch in
// pkg/controlplane/runtime.CreateMetadataStoreFromConfig is what actually
// decides whether a store can be built; a type missing here is one an operator
// cannot create or edit interactively even though the server would take it.
//
// postgres is absent because its branch prompts for connection settings and
// would block on stdin. The types below all take their config from flags.
func TestBuildMetadataConfig_AcceptsServerSupportedTypes(t *testing.T) {
	for _, storeType := range []string{"memory", "badger", "sqlite"} {
		t.Run(storeType, func(t *testing.T) {
			if _, err := buildMetadataConfig(storeType, "", t.TempDir()); err != nil {
				t.Errorf("server supports %q but the CLI rejects it: %v", storeType, err)
			}
		})
	}

	if _, err := buildMetadataConfig("nonesuch", "", ""); err == nil {
		t.Error("an unsupported type should be rejected, got nil error")
	}
}
