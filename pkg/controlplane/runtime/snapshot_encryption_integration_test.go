package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/encryption"
	"github.com/marmos91/dittofs/pkg/blockstore/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	bsmemory "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// TestSnapshot_EncryptionInteraction validates the snapshot
// manifest / verify / restore path against an encryption-enabled remote
// (issue #816). The encryption decorator stores framed CIPHERTEXT in the
// inner remote but keeps the PLAINTEXT BLAKE3 as the CAS storage key:
// dedup, GC, the snapshot manifest, and the verify HEAD-probe must all
// operate on the plaintext content hash, never on a ciphertext-derived
// key.
//
// The sub-tests assert, against a real EncryptedRemote wired into a real
// engine.Store + the production CreateSnapshot orchestration:
//
//   - Manifest entries are the plaintext content hashes used to Put, so
//     the hashes recorded on disk address the encrypted blocks correctly.
//   - The verify gate's per-hash Head probe resolves every manifest hash
//     against the encrypted remote (Head parses the wire frame to derive
//     plaintext size) and the snapshot reaches ready + remote-durable.
//   - Multi-chunk blocks round-trip byte-for-byte back through the
//     decorator's authenticated decrypt — the restore-time read path
//     reconstructs the exact plaintext.
//   - A manifest hash whose block is present in the inner store but
//     UNFRAMED (e.g. written bypassing the encryption decorator) fails
//     verify rather than masquerading as durable — the probe really hits
//     the encrypted identity, not a raw presence check.
func TestSnapshot_EncryptionInteraction(t *testing.T) {
	t.Run("ManifestHashesArePlaintextAndVerifyProbesEncryptedRemote", testEncryptionManifestAndVerify)
	t.Run("MultiChunkRoundTripBytesCorrect", testEncryptionMultiChunkRoundTrip)
	t.Run("UnframedBlockUnderManifestHashFailsVerify", testEncryptionUnframedFailsVerify)
}

// encryptedFixture is the orchestration fixture wired with a real
// EncryptedRemote (local passphrase key provider) in front of the memory
// remote. inner is retained so a test can write an unframed body straight
// to the underlying store, bypassing the decorator's framing.
type encryptedFixture struct {
	*orchestrationFixture
	inner *remotememory.Store
	enc   *encryption.EncryptedRemote
}

// newEncryptedFixture mirrors newOrchestrationFixture but interposes the
// encryption decorator between the engine and the memory remote so the
// orchestration's bs.RemoteStore() (used by the verify gate) is the
// EncryptedRemote.
func newEncryptedFixture(t *testing.T) *encryptedFixture {
	t.Helper()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	mem := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	backup := &controlledBackupable{MemoryMetadataStore: mem}
	if err := rt.RegisterMetadataStore("memory", backup); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if _, err := cp.CreateMetadataStore(context.Background(), &models.MetadataStoreConfig{
		Name: "memory",
		Type: "memory",
	}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}

	localStoreDir := t.TempDir()
	shareName := "data"

	localStore := bsmemory.New()
	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })

	enc := newEncryptedRemote(t, inner)

	syncer := engine.NewSyncer(localStore, enc, mem, engine.SyncerConfig{
		ParallelUploads:   1,
		ParallelDownloads: 1,
	})
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         enc,
		Syncer:         syncer,
		FileBlockStore: mem,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: "memory",
		BlockStore:    bs,
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	return &encryptedFixture{
		orchestrationFixture: &orchestrationFixture{
			t:             t,
			rt:            rt,
			store:         cp,
			backup:        backup,
			bs:            bs,
			localStoreDir: localStoreDir,
			shareName:     shareName,
		},
		inner: inner,
		enc:   enc,
	}
}

// newEncryptedRemote builds an EncryptedRemote backed by a fresh local
// passphrase-protected key file, wrapping inner. The key file + passphrase
// are scoped to this test only.
func newEncryptedRemote(t *testing.T, inner *remotememory.Store) *encryption.EncryptedRemote {
	t.Helper()

	const passphrase = "snapshot-encryption-e2e-passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	keyBytes, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	provider, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{
		Kind: keyprovider.KindLocal,
		File: keyPath,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	enc, err := encryption.NewRemote(inner, encryption.EncryptionPolicy{
		AEAD: encryption.AEADAES256GCM,
	}, provider)
	if err != nil {
		t.Fatalf("encryption.NewRemote: %v", err)
	}
	return enc
}

// seedEncrypted Puts each payload through the encryption decorator under
// its plaintext content hash and drives the orchestration's backup
// HashSet to exactly those hashes.
func (f *encryptedFixture) seedEncrypted(payloads [][]byte) []blockstore.ContentHash {
	f.t.Helper()
	hashes := make([]blockstore.ContentHash, 0, len(payloads))
	for _, p := range payloads {
		h := blockstore.ContentHash(blake3.Sum256(p))
		if err := f.enc.Put(context.Background(), h, p); err != nil {
			f.t.Fatalf("encrypted Put: %v", err)
		}
		hashes = append(hashes, h)
	}
	f.setBackupHashes(hashes)
	return hashes
}

func testEncryptionManifestAndVerify(t *testing.T) {
	fx := newEncryptedFixture(t)
	defer fx.close()

	payloads := [][]byte{
		bytes.Repeat([]byte{0x11}, 4096),
		bytes.Repeat([]byte{0x22}, 8192),
		append([]byte("mixed-content-block"), bytes.Repeat([]byte{0x33}, 1000)...),
	}
	hashes := fx.seedEncrypted(payloads)

	// Sanity: the decorator round-trips the plaintext, proving the inner
	// store holds framed ciphertext keyed by the plaintext content hash.
	rawFramed, err := fx.enc.Get(context.Background(), hashes[0])
	if err != nil {
		t.Fatalf("decorator Get (sanity): %v", err)
	}
	if !bytes.Equal(rawFramed, payloads[0]) {
		t.Fatalf("decorator Get returned wrong plaintext for hash[0]")
	}

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v (verify gate must HEAD-probe the encrypted remote successfully)", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want %q", snap.State, models.StateReady)
	}
	if !snap.RemoteDurable {
		t.Fatal("snap.RemoteDurable = false, want true: verify probed the encrypted remote by content hash and every block was present")
	}

	// The manifest on disk must record the PLAINTEXT content hashes — the
	// same identity used to Put and to HEAD-probe — not any ciphertext key.
	manifestPath := snap.ManifestPath(fx.localStoreDir)
	mf, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = mf.Close() }()
	manifest, err := snapshot.ReadManifest(mf)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if manifest.Len() != len(hashes) {
		t.Fatalf("manifest len = %d, want %d", manifest.Len(), len(hashes))
	}
	for _, h := range hashes {
		if !manifest.Contains(h) {
			t.Fatalf("manifest missing plaintext content hash %s", h)
		}
	}
}

func testEncryptionMultiChunkRoundTrip(t *testing.T) {
	fx := newEncryptedFixture(t)
	defer fx.close()

	// A 4MB file split into 1MB blocks — mirrors the #789 multi-pass
	// validation shape. Each block carries distinct content so dedup does
	// not collapse them and every chunk must decrypt independently.
	const blockSize = 1 << 20
	const blocks = 4
	payloads := make([][]byte, blocks)
	for i := range payloads {
		b := make([]byte, blockSize)
		for j := range b {
			b[j] = byte((i*7 + j) % 251)
		}
		payloads[i] = b
	}
	hashes := fx.seedEncrypted(payloads)

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady || !snap.RemoteDurable {
		t.Fatalf("snap state=%q durable=%v, want ready+durable", snap.State, snap.RemoteDurable)
	}

	// Restore-time reads pull each manifest block back through the
	// decorator. Assert byte-for-byte plaintext recovery + that the
	// re-derived hash equals the manifest identity (ReadBlockVerified).
	for i, h := range hashes {
		got, err := fx.enc.ReadBlockVerified(ctx, h, h)
		if err != nil {
			t.Fatalf("ReadBlockVerified block %d (%s): %v", i, h, err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Fatalf("block %d round-trip mismatch: got %d bytes, want %d", i, len(got), len(payloads[i]))
		}
	}
}

func testEncryptionUnframedFailsVerify(t *testing.T) {
	fx := newEncryptedFixture(t)
	defer fx.close()

	// Seed two real encrypted blocks, then inject a THIRD block whose body
	// was written straight to the inner remote WITHOUT the encryption frame
	// (simulating an externally-mutated / pre-encryption block). The
	// manifest lists its plaintext hash, but the verify HEAD-probe must
	// reject it: EncryptedRemote.Head parses the wire frame and returns a
	// non-NotFound error for an unframed body, so verify fails rather than
	// reporting hollow durability over a block it cannot decrypt.
	good := [][]byte{
		bytes.Repeat([]byte{0xa1}, 2048),
		bytes.Repeat([]byte{0xb2}, 2048),
	}
	hashes := fx.seedEncrypted(good)

	unframed := bytes.Repeat([]byte{0xc3}, 2048)
	unframedHash := blockstore.ContentHash(blake3.Sum256(unframed))
	if err := fx.inner.Put(context.Background(), unframedHash, unframed); err != nil {
		t.Fatalf("inner Put (unframed): %v", err)
	}

	// Manifest = two framed blocks + the unframed one.
	allHashes := append(append([]blockstore.ContentHash{}, hashes...), unframedHash)
	fx.setBackupHashes(allHashes)

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotVerifyFailed)", werr)
	}
	if snap == nil || snap.State != models.StateFailed {
		t.Fatalf("snap = %+v, want state=failed", snap)
	}
}
