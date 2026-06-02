package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// encryptedRemoteCfg builds a remote BlockStoreConfig (kind=remote, type=memory)
// whose Config JSON carries an "encryption" block, so the real share-build path
// (shares.maybeWrapEncryption) interposes an EncryptedRemote in front of the
// in-memory remote. The master key is a fresh passphrase-protected local key
// file scoped to this test.
func encryptedRemoteCfg(t *testing.T) *models.BlockStoreConfig {
	t.Helper()

	const passphrase = "byteverify-encrypted-remote-passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	keyBytes, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	cfg := &models.BlockStoreConfig{
		Name: "bv-enc-remote",
		Kind: models.BlockStoreKindRemote,
		Type: "memory",
	}
	// AEAD omitted → defaults to AES-256-GCM (encryption.ParsePolicy).
	if err := cfg.SetConfig(map[string]any{
		"encryption": map[string]any{
			"key": map[string]any{
				"kind": string(keyprovider.KindLocal),
				"file": keyPath,
			},
		},
	}); err != nil {
		t.Fatalf("SetConfig(encryption): %v", err)
	}
	return cfg
}

// TestSnapshotByteVerify_EncryptedRemote_Matrix is the encrypted-remote edge
// case: a full write → durable snapshot → mutate → ResetLocalState → restore →
// byte-compare cycle, but with an encryption-enabled remote block store, run
// across every metadata backend (memory + badger always; postgres when the
// integration companion installed it).
//
// This pins the intersection that the existing tests cover only separately:
//   - TestSnapshot_EncryptionInteraction proves the encrypted verify/decrypt
//     path, but only with memory metadata + a controlled HashSet.
//   - TestSnapshotByteVerify_Matrix proves real per-backend metadata
//     Backup/restore byte round-trips, but only over a plaintext store.
//
// Here both hold at once: the snapshot manifest is the plaintext content
// hashes, the durability verify HEAD-probes the EncryptedRemote, the metadata
// dump is the real per-backend serialization, and the post-ResetLocalState
// restore re-reads every block back through the AEAD decrypt — byte-identical.
func TestSnapshotByteVerify_EncryptedRemote_Matrix(t *testing.T) {
	for _, bk := range byteVerifyBackends(t) {
		bk := bk
		t.Run(bk.name, func(t *testing.T) {
			if bk.skip != "" {
				t.Skip(bk.skip)
			}
			meta, metaType := bk.open(t)
			fx := newByteVerifyFixtureOpts(t, meta, metaType, encryptedRemoteCfg(t))
			defer fx.close()
			runEncryptedDurableCycle(t, fx)
		})
	}
}

// runEncryptedDurableCycle mirrors runByteVerifyCycle but drives a REMOTE-DURABLE
// snapshot (no NoVerify) so the verify gate HEAD-probes the encrypted remote,
// and reads after ResetLocalState resolve cold through the encrypted CAS.
func runEncryptedDurableCycle(t *testing.T, fx *byteVerifyFixture) {
	ctx := context.Background()
	const mib = 1 << 20

	origA := distinctBytes(3*mib, 0xA) // multi-chunk
	origB := distinctBytes(8192, 0xB)

	pidA := fx.createEmptyFile(ctx, "fileA.bin")
	fx.writeFile(ctx, pidA, origA)
	pidB := fx.createEmptyFile(ctx, "fileB.bin")
	fx.writeFile(ctx, pidB, origB)

	if fa := fx.getFile(ctx, "fileA.bin"); len(fa.Blocks) < 2 {
		t.Fatalf("fileA produced %d CAS block(s), want >= 2 (multi-chunk path not exercised)", len(fa.Blocks))
	}

	// Durable snapshot: create drains uploads to the encrypted remote, then the
	// verify gate HEAD-probes every manifest hash against the EncryptedRemote.
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v (verify gate must HEAD-probe the encrypted remote)", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want ready", snap.State)
	}
	if !snap.RemoteDurable {
		t.Fatal("snap.RemoteDurable = false: durability verify against the encrypted remote did not confirm every block")
	}

	// Mutate: overwrite fileA in place (same length), delete fileB.
	mutA := distinctBytes(3*mib, 0xA11)
	if len(mutA) != len(origA) {
		t.Fatalf("test bug: mutA len %d != origA len %d", len(mutA), len(origA))
	}
	fx.writeFile(ctx, pidA, mutA)
	fx.deleteFile(ctx, "fileB.bin")

	// Drop ALL local state so restore reads must fetch+decrypt from the
	// encrypted remote (the cold encrypted-CAS read path).
	if err := fx.bs.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	// Restore (durable → AllowNonDurable not required).
	if err := fx.rt.DisableShare(ctx, fx.shareName); err != nil {
		t.Fatalf("DisableShare: %v", err)
	}
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	// fileA rolled back to original bytes; fileB recovered — both byte-identical
	// after decrypting from the encrypted remote.
	if !fx.fileExists(ctx, "fileA.bin") {
		t.Fatal("fileA missing after restore")
	}
	if got := fx.readFile(ctx, pidA, len(origA)); !bytes.Equal(got, origA) {
		t.Fatalf("RESTORE fileA NOT byte-identical (encrypted cold read): %s", firstDiff(origA, got))
	}
	if !fx.fileExists(ctx, "fileB.bin") {
		t.Fatal("fileB not recovered after restore")
	}
	pidB2 := fx.getFile(ctx, "fileB.bin").PayloadID
	if got := fx.readFile(ctx, pidB2, len(origB)); !bytes.Equal(got, origB) {
		t.Fatalf("RESTORE fileB NOT byte-identical (encrypted cold read): %s", firstDiff(origB, got))
	}
}
