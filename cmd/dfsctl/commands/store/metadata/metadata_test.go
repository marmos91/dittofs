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
