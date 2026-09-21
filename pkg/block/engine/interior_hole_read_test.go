package engine

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// TestReadAt_InteriorHoleHydratesRatherThanServingZeros pins the read path over
// a range the local tier never received but the manifest places.
//
// The tier holds the LATER chunk and nothing before it. Its buffer therefore
// spans both, because a write landing past the end zero-fills the gap under it,
// and those zeros are byte-identical to a chunk written as zeros. A read of the
// earlier range lands entirely inside that buffer, so nothing about its length
// says the bytes are missing.
//
// The engine reconciles on the local tier's hole flag, so that flag is the only
// thing standing between this read and a full-length success made of zeros —
// no error at any layer, the silent-hole failure. It must come back as the
// remote's bytes, hydrated on demand.
//
// The later chunk is read too: it is present locally and correct either way, so
// a failure confined to the earlier range is the defect rather than a broken
// fixture.
func TestReadAt_InteriorHoleHydratesRatherThanServingZeros(t *testing.T) {
	for _, b := range manifestBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			bs, fbs, rs, shs := newRemoteBackedEngine(t, b)
			const (
				payloadID = "payload-interior-hole"
				chunkSize = 4096
			)

			early := make([]byte, chunkSize)
			late := make([]byte, chunkSize)
			rand.New(rand.NewSource(0x11A7)).Read(early) //nolint:gosec // deterministic fixture
			rand.New(rand.NewSource(0x11A8)).Read(late)  //nolint:gosec // deterministic fixture

			// Both chunks are remote-durable and both rows are in the manifest.
			seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, 0, early)
			seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, chunkSize, late)

			// Only the later one ever reached the local tier — a cold read that
			// hydrated the tail while the head was never fetched.
			if err := bs.Local().Hydrate(ctx, journal.FileID(payloadID), chunkSize, late, 0); err != nil {
				t.Fatalf("hydrate later chunk: %v", err)
			}

			// The gap is inside the buffer, so its length cannot betray it.
			if size, ok := bs.Local().FileSize(ctx, journal.FileID(payloadID)); !ok || size != 2*chunkSize {
				t.Fatalf("local FileSize = (%d, %v); want (%d, true) — the fixture must leave "+
					"the hole INSIDE the buffer or it proves nothing", size, ok, 2*chunkSize)
			}

			// Poison the destination so an unhydrated range fails loudly instead
			// of matching the zeros it would be served.
			got := bytes.Repeat([]byte{0xAA}, chunkSize)
			n, err := bs.ReadAt(ctx, payloadID, got, 0)
			if err != nil {
				t.Fatalf("ReadAt over the interior hole: %v", err)
			}
			if n != chunkSize {
				t.Fatalf("ReadAt n = %d, want %d", n, chunkSize)
			}
			if bytes.Equal(got, make([]byte, chunkSize)) {
				t.Fatalf("ReadAt returned %d zero bytes over a range the manifest places: the "+
					"tier called the zero fill data and the engine never hydrated", chunkSize)
			}
			if !bytes.Equal(got, early) {
				t.Errorf("ReadAt returned the wrong bytes over the interior hole")
			}

			// The range that was present all along must still read correctly.
			gotLate := bytes.Repeat([]byte{0xBB}, chunkSize)
			if _, err := bs.ReadAt(ctx, payloadID, gotLate, chunkSize); err != nil {
				t.Fatalf("ReadAt over the resident range: %v", err)
			}
			if !bytes.Equal(gotLate, late) {
				t.Errorf("the resident range read back wrong; the fixture is broken, not the defect")
			}
		})
	}
}
