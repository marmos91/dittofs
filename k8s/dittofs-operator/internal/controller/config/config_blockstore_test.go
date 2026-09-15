package config

import (
	"strings"
	"testing"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// TestGenerateDittoFSConfig_Blockstore verifies the rendered
// blockstore.journal.path tracks the content PVC: it names the mount the PVC is
// attached at, so a share's journal — the only copy of a byte until its block
// store has it — survives a reschedule instead of dying with the pod.
func TestGenerateDittoFSConfig_Blockstore(t *testing.T) {
	t.Run("no content size -> no blockstore block", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Storage: dittoiov1alpha1.StorageSpec{MetadataSize: "10Gi"},
			},
		}
		if b := buildBlockstoreConfig(ds); b != nil {
			t.Fatalf("expected nil blockstore config, got %+v", b)
		}
		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}
		// Nothing is mounted at that path, so naming it would point the journal
		// at a directory the operator did not provision.
		if strings.Contains(yamlStr, "blockstore:") {
			t.Fatalf("rendered config should omit the blockstore block:\n%s", yamlStr)
		}
	})

	t.Run("content size -> journal path is the content mount", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Storage: dittoiov1alpha1.StorageSpec{
					MetadataSize: "10Gi",
					ContentSize:  "50Gi",
				},
			},
		}

		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}

		// Re-parse to assert the rendered keys.
		var parsed struct {
			Blockstore *BlockstoreConfig `yaml:"blockstore"`
		}
		if err := yaml.Unmarshal([]byte(yamlStr), &parsed); err != nil {
			t.Fatalf("re-parse rendered config: %v", err)
		}
		if parsed.Blockstore == nil {
			t.Fatalf("expected blockstore block in:\n%s", yamlStr)
		}
		if got := parsed.Blockstore.Journal.Path; got != dittoiov1alpha1.BlockMountPath {
			t.Errorf("blockstore.journal.path = %q, want the content mount %q", got, dittoiov1alpha1.BlockMountPath)
		}
	})
}
