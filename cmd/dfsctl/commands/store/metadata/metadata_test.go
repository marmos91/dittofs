package metadata

import (
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup"
)

// TestMetadataCmd_RegistersBackupSubtree verifies that the Phase 6 backup
// subtree is wired into metadata.Cmd. The backup subtree owns repo, restore,
// run, list, show, pin, unpin, and job verbs.
func TestMetadataCmd_RegistersBackupSubtree(t *testing.T) {
	found := false
	for _, c := range Cmd.Commands() {
		if c.Name() == backup.Cmd.Name() {
			found = true
		}
	}
	if !found {
		t.Errorf("metadata.Cmd does not register backup subtree")
	}
}

// TestBackupSubtree_ExposesPhase6Verbs verifies that the backup subtree
// exposes every Phase 6 verb (run / list / show / pin / unpin / restore /
// repo / job). Guards against accidental drops after refactors.
func TestBackupSubtree_ExposesPhase6Verbs(t *testing.T) {
	required := []string{"run", "list", "show", "pin", "unpin", "restore", "repo", "job"}
	for _, want := range required {
		if _, _, err := backup.Cmd.Find([]string{want}); err != nil {
			t.Errorf("backup subtree missing verb %q: %v", want, err)
		}
	}
}

// TestMetadataCmd_RegistersExistingVerbs guards against regressions where
// Phase 6 wiring accidentally displaces pre-existing verbs.
func TestMetadataCmd_RegistersExistingVerbs(t *testing.T) {
	required := []string{"list", "add", "edit", "remove", "health"}
	for _, want := range required {
		if _, _, err := Cmd.Find([]string{want}); err != nil {
			t.Errorf("existing verb %q lost from metadata.Cmd after Phase 6 wiring: %v", want, err)
		}
	}
}
