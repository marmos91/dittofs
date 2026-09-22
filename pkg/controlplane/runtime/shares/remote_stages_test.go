package shares

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/middleware/compression"
	"github.com/marmos91/dittofs/pkg/block/middleware/encryption"
	"github.com/marmos91/dittofs/pkg/block/middleware/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRemoteStages_CompressionBeforeEncryption pins the production stage order.
//
// The pipeline runs stages in the order this slice holds them, and it cannot
// check that order itself: both arrangements round-trip and neither errors. The
// only symptom of a swap is that every block is compressed after encryption, at
// a ratio of ~1.0, forever. middleware.TestPipeline_CompressBeforeEncrypt shows
// that the two orders differ; this test is the one that says which order ships.
func TestRemoteStages_CompressionBeforeEncryption(t *testing.T) {
	cfg := bothStagesConfig(t)

	stages, err := remoteStages(context.Background(), cfg)
	if err != nil {
		t.Fatalf("remoteStages: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range stages {
			if c, ok := s.(interface{ Close() error }); ok {
				_ = c.Close()
			}
		}
	})

	if len(stages) != 2 {
		t.Fatalf("got %d stages, want 2 (compression and encryption)", len(stages))
	}
	if _, ok := stages[0].(*compression.Transform); !ok {
		t.Errorf("stage 0 is %T, want *compression.Transform: compression must seal first, before the bytes become incompressible ciphertext", stages[0])
	}
	if _, ok := stages[1].(*encryption.Transform); !ok {
		t.Errorf("stage 1 is %T, want *encryption.Transform", stages[1])
	}
}

// TestRemoteStages_OmitsAbsentStages pins that a config naming neither key
// yields no pipeline at all, so an unencrypted, uncompressed share keeps
// talking to its remote store directly.
func TestRemoteStages_OmitsAbsentStages(t *testing.T) {
	stages, err := remoteStages(context.Background(), &models.BlockStoreConfig{Config: "{}"})
	if err != nil {
		t.Fatalf("remoteStages: %v", err)
	}
	if len(stages) != 0 {
		t.Fatalf("got %d stages for an empty config, want 0", len(stages))
	}
}

// bothStagesConfig builds a BlockStoreConfig naming compression and encryption,
// with a real local key file so the encryption stage can actually be built.
func bothStagesConfig(t *testing.T) *models.BlockStoreConfig {
	t.Helper()
	const passphrase = "remote-stages-test-passphrase"

	raw, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "share.key")
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	body, err := json.Marshal(map[string]any{
		"compression": map[string]any{"algo": "zstd"},
		"encryption": map[string]any{
			"aead": "aes-256-gcm",
			"key":  map[string]any{"kind": "local", "file": keyPath},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return &models.BlockStoreConfig{Config: string(body)}
}
