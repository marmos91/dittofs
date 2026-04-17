package metadata

import (
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/repo"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/restore"
)

// TestMetadataCmd_RegistersPhase6Subtrees verifies that the three Phase 6
// subtrees (backup / repo / restore) are wired into metadata.Cmd. The parent
// explicitly owns the AddCommand calls — the leaf packages cannot self-wire
// without creating an import cycle through metadata.go.
func TestMetadataCmd_RegistersPhase6Subtrees(t *testing.T) {
	want := map[string]bool{
		backup.Cmd.Name():  false,
		repo.Cmd.Name():    false,
		restore.Cmd.Name(): false,
	}
	for _, c := range Cmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metadata.Cmd does not register subtree %q", name)
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
