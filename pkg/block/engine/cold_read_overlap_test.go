package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestEnsureAvailableAndRead_StraddlerDoesNotShadowLaterRow pins what a cold
// read does with two manifest rows that overlap.
//
// Coverage resolves an overlap to the greatest start, so from a later row's
// first byte it is that row that holds the bytes and the straddling row's
// remaining extent describes bytes it no longer owns. The fetch path has to
// agree: chunks download concurrently and the local tier keeps whichever
// hydrate landed last, so a straddler writing its full extent leaves its
// pre-overwrite bytes over the newer row's head — served with no error once the
// newer bytes have been evicted, which is the whole point of the cold path.
//
// Two shapes of overlap, because they fail differently. When the later row
// merely starts inside the straddler, both rows are fetched and the corruption
// is decided by which download finished last. When the later row is fully
// contained, a walk that consumed the straddler's extent never resolves the
// contained row at all, so it is never fetched.
//
// A third shape parks an unplaceable row in the same manifest. Bounding a row's
// claim needs the successor lookup, and the offset index refuses that question
// outright once any row cannot be placed — a refusal that must not be read as
// "nothing starts later", which is the row's full extent and the very claim the
// bound exists to narrow.
//
// Both store shapes are exercised because they resolve coverage and succession
// by different means — an offset index versus a manifest walk.
func TestEnsureAvailableAndRead_StraddlerDoesNotShadowLaterRow(t *testing.T) {
	const oneMiB = 1024 * 1024

	for _, shape := range []struct {
		name        string
		laterOff    uint64
		laterSize   int
		unplaceable bool
	}{
		{name: "LaterRowStartsInside", laterOff: 4 * oneMiB, laterSize: 2 * oneMiB},
		{name: "LaterRowFullyContained", laterOff: 2 * oneMiB, laterSize: oneMiB},
		{name: "AlongsideAnUnplaceableRow", laterOff: 4 * oneMiB, laterSize: 2 * oneMiB, unplaceable: true},
	} {
		for _, backend := range manifestBackends() {
			t.Run(shape.name+"/"+backend.name, func(t *testing.T) {
				ctx := context.Background()
				payloadID := "payload-overlapping-rows"

				const straddlerOff, straddlerSize = oneMiB, 4 * oneMiB
				straddler := bytes.Repeat([]byte{0x5A}, straddlerSize)
				later := bytes.Repeat([]byte{0xC3}, shape.laterSize)

				loc := memorylocal.New()
				rs := remotememory.New()
				fbs, shs := backend.build(t)

				seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, straddlerOff, straddler)
				seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, shape.laterOff, later)
				if shape.unplaceable {
					if err := fbs.Put(ctx, &block.FileChunk{ID: payloadID + "/not-an-offset", DataSize: 4096}); err != nil {
						t.Fatalf("seed unplaceable row: %v", err)
					}
				}

				m := newFetchSyncer(loc, rs, fbs, shs)

				laterEnd := shape.laterOff + uint64(shape.laterSize)
				end := uint64(straddlerOff + straddlerSize)
				if laterEnd > end {
					end = laterEnd
				}
				dest := make([]byte, end-straddlerOff)
				if err := m.EnsureAvailableAndRead(ctx, payloadID, straddlerOff, uint32(len(dest)), dest); err != nil {
					t.Fatalf("EnsureAvailableAndRead: %v", err)
				}

				// The straddler's bytes are expected everywhere it is still the
				// greatest start, the later row's everywhere it is.
				want := make([]byte, end-straddlerOff)
				copy(want, straddler)
				copy(want[shape.laterOff-straddlerOff:], later)

				// Poison the destination so an unhydrated range fails the compare
				// instead of hiding in zeros.
				got := bytes.Repeat([]byte{0xAA}, len(want))
				_, st, err := loc.ReadAt(ctx, payloadID, int64(straddlerOff), got)
				if err != nil {
					t.Fatalf("local ReadAt after fetch: %v", err)
				}
				if st.Cold {
					t.Fatal("local ReadAt reports cold after fetch: a covering row was never hydrated")
				}
				if !bytes.Equal(got, want) {
					at, wantByte, gotByte := 0, byte(0), byte(0)
					for i := range want {
						if want[i] != got[i] {
							at, wantByte, gotByte = i, want[i], got[i]
							break
						}
					}
					t.Fatalf("hydrated bytes differ at offset %d: got %#x, want %#x",
						straddlerOff+uint64(at), gotByte, wantByte)
				}
			})
		}
	}
}
