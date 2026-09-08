package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/engine"
	bsmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
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
// dedup, GC, the snapshot manifest, and the verify probe must all
// operate on the plaintext content hash, never on a ciphertext-derived
// key.
//
// The sub-tests assert, against a real EncryptedRemote wired into a real
// engine.Store + the production CreateSnapshot orchestration:
//
//   - Manifest entries are the plaintext content hashes used to seal, so
//     the hashes recorded on disk address the encrypted blocks correctly.
//   - The verify gate's per-hash probe reads every manifest chunk back
//     through ReadChunk (deframe + authenticated decrypt) against the
//     encrypted remote, and the snapshot reaches ready + remote-durable.
//   - Multi-chunk blocks round-trip byte-for-byte back through the
//     decorator's authenticated decrypt — the restore-time read path
//     reconstructs the exact plaintext.
//   - A manifest hash whose block is present in the packed keyspace but
//     UNFRAMED (e.g. written bypassing the encryption decorator) fails
//     verify rather than masquerading as durable — the probe decrypt-
//     verifies the chunk, it is not a raw presence check.
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
	backup := &controlledSnapshotable{MemoryMetadataStore: mem}
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
		ParallelDownloads: 1,
	})
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         enc,
		Syncer:         syncer,
		FileChunkStore: mem,
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

// seedEncrypted seals each payload through the encryption decorator under
// its plaintext content hash and drives the orchestration's backup
// HashSet to exactly those hashes.
func (f *encryptedFixture) seedEncrypted(payloads [][]byte) []block.ContentHash {
	f.t.Helper()
	hashes := make([]block.ContentHash, 0, len(payloads))
	for _, p := range payloads {
		h := block.ContentHash(blake3.Sum256(p))
		// Durability is block-only: seed a real sealed frame as the packed
		// block so the verify gate's ReadChunk probe deframes and
		// decrypt-verifies it. seedBlockFrame records the frame's wire extent
		// on the hash's locator.
		f.seedBlockFrame(h, p)
		hashes = append(hashes, h)
	}
	f.backup.setHashes(hashes)
	return hashes
}

// readChunk reads h's block back through the encryption decorator using the
// locator seedBlockFrame recorded — the restore-time read path: range-read the
// frame out of the packed block, deframe, authenticated-decrypt against h.
func (f *encryptedFixture) readChunk(ctx context.Context, h block.ContentHash) ([]byte, error) {
	f.t.Helper()
	loc, ok, err := f.backup.GetLocator(ctx, h)
	if err != nil || !ok {
		f.t.Fatalf("GetLocator(%s): ok=%v err=%v", h, ok, err)
	}
	return f.enc.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, h)
}

// seedBlockFrame seals plaintext into an encryption frame, stores it as the
// packed block object under blockID = hash, and marks the hash synced to a
// locator carrying the frame's wire extent (WireLength) so the verify gate
// reads the chunk back through ReadChunk rather than a bare presence probe.
func (f *encryptedFixture) seedBlockFrame(h block.ContentHash, plaintext []byte) {
	f.t.Helper()
	wire, err := f.enc.SealChunk(context.Background(), h, plaintext)
	if err != nil {
		f.t.Fatalf("SealChunk: %v", err)
	}
	if err := f.inner.PutBlock(context.Background(), h.String(), bytes.NewReader(wire)); err != nil {
		f.t.Fatalf("PutBlock (frame): %v", err)
	}
	if err := f.backup.MarkSynced(context.Background(), h, block.ChunkLocator{
		BlockID:    h.String(),
		WireLength: int64(len(wire)),
	}); err != nil {
		f.t.Fatalf("MarkSynced: %v", err)
	}
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

	// Sanity: the block sitting in the inner store is NOT the plaintext (it is
	// a framed ciphertext at rest), yet reading it back through the decorator
	// under the plaintext content hash yields the plaintext exactly — the
	// storage key is the plaintext BLAKE3, the stored bytes are not.
	atRest, err := fx.inner.GetBlock(context.Background(), hashes[0].String())
	if err != nil {
		t.Fatalf("inner GetBlock (sanity): %v", err)
	}
	if bytes.Contains(atRest, payloads[0]) {
		t.Fatalf("block for hash[0] is stored as plaintext; want encrypted at rest")
	}
	plain, err := fx.readChunk(context.Background(), hashes[0])
	if err != nil {
		t.Fatalf("decorator ReadChunk (sanity): %v", err)
	}
	if !bytes.Equal(plain, payloads[0]) {
		t.Fatalf("decorator ReadChunk returned wrong plaintext for hash[0]")
	}

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v (verify gate must read every chunk back through the encrypted remote successfully)", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want %q", snap.State, models.StateReady)
	}
	if !snap.RemoteDurable {
		t.Fatal("snap.RemoteDurable = false, want true: verify probed the encrypted remote by content hash and every block was present")
	}

	// The manifest on disk must record the PLAINTEXT content hashes — the
	// same identity used to seal and to probe — not any ciphertext key.
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
	// re-derived hash equals the manifest identity.
	for i, h := range hashes {
		got, err := fx.readChunk(ctx, h)
		if err != nil {
			t.Fatalf("ReadChunk block %d (%s): %v", i, h, err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Fatalf("block %d round-trip mismatch: got %d bytes, want %d", i, len(got), len(payloads[i]))
		}
		if regot := block.ContentHash(blake3.Sum256(got)); regot != h {
			t.Fatalf("block %d re-derived hash %s != manifest identity %s", i, regot, h)
		}
	}
}

func testEncryptionUnframedFailsVerify(t *testing.T) {
	fx := newEncryptedFixture(t)
	defer fx.close()

	// Seed two real encrypted blocks, then inject a THIRD packed block whose
	// body was written WITHOUT the encryption frame (simulating an externally-
	// mutated / pre-encryption block) under a valid block locator. The manifest
	// lists its hash, but the verify gate reads each chunk back through
	// ReadChunk: deframing the raw body fails (ErrCiphertextWithoutFrame), so
	// verify fails rather than reporting hollow durability over a block it
	// cannot decrypt.
	good := [][]byte{
		bytes.Repeat([]byte{0xa1}, 2048),
		bytes.Repeat([]byte{0xb2}, 2048),
	}
	hashes := fx.seedEncrypted(good)

	unframed := bytes.Repeat([]byte{0xc3}, 2048)
	unframedHash := block.ContentHash(blake3.Sum256(unframed))
	// Present in the PACKED keyspace under a valid locator, but not a frame.
	if err := fx.inner.PutBlock(context.Background(), unframedHash.String(), bytes.NewReader(unframed)); err != nil {
		t.Fatalf("inner PutBlock (unframed): %v", err)
	}
	if err := fx.backup.MarkSynced(context.Background(), unframedHash, block.ChunkLocator{
		BlockID:    unframedHash.String(),
		WireLength: int64(len(unframed)),
	}); err != nil {
		t.Fatalf("MarkSynced (unframed): %v", err)
	}

	// Manifest = two framed blocks + the unframed one.
	allHashes := append(append([]block.ContentHash{}, hashes...), unframedHash)
	fx.backup.setHashes(allHashes)

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
