package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// assertNoOverlap fails when two manifest rows claim the same byte. Unlike
// assertManifestTiles it tolerates gaps, which are legitimate for a sparse file,
// and checks only the half the covering lookup cannot resolve on its own.
func overlappedOffset(t *testing.T, ms metadata.Store, pid string) (uint64, bool) {
	t.Helper()
	refs := manifestRefs(t, ms, pid)
	sort.Slice(refs, func(i, j int) bool { return refs[i].Offset < refs[j].Offset })
	var prevEnd uint64
	var prevOff uint64
	for i, r := range refs {
		if i > 0 && r.Offset < prevEnd {
			t.Logf("manifest OVERLAP — row at %d extends to %d, next row starts at %d", prevOff, prevEnd, r.Offset)
			return r.Offset, true
		}
		prevOff, prevEnd = r.Offset, r.Offset+uint64(r.Size)
	}
	return 0, false
}

// soakModel mirrors the byte content the engine should hold.
type soakModel struct{ b []byte }

func (m *soakModel) write(off uint64, data []byte) {
	if need := int(off) + len(data); need > len(m.b) {
		m.b = append(m.b, make([]byte, need-len(m.b))...)
	}
	copy(m.b[off:], data)
}

func (m *soakModel) truncate(size uint64) {
	if int(size) <= len(m.b) {
		m.b = m.b[:size]
		return
	}
	m.b = append(m.b, make([]byte, int(size)-len(m.b))...)
}

func (m *soakModel) punch(off, length uint64) {
	end := off + length
	if end > uint64(len(m.b)) {
		end = uint64(len(m.b))
	}
	for i := off; i < end; i++ {
		m.b[i] = 0
	}
}

// TestManifestSoak_NoLegitimatePathOverlaps drives the real mutation paths
// through randomized sequences and, after every carve, asserts that no two
// manifest rows cover the same offset and that a cold read of the whole file
// still matches the model. The covering lookup reports overlapping rows as an
// inconsistency rather than resolving them, so a legitimate path that produced
// overlap would turn readable files into read errors.
func TestManifestSoak_NoLegitimatePathOverlaps(t *testing.T) {
	if testing.Short() {
		t.Skip("soak")
	}
	for seed := range 24 {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			runManifestSoak(t, metadatamemory.NewMemoryMetadataStoreWithDefaults(), 60, int64(seed))
		})
	}
}

func runManifestSoak(t *testing.T, ms metadata.Store, iterations int, seedN int64) {
	t.Helper()
	ctx := context.Background()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	share := fmt.Sprintf("soak%d", seedN)
	rootHandle := createShare(t, ms, share)
	pid, _ := createRealFile(t, ms, share, "soak.bin", rootHandle)

	const maxSize = 12 * 1024 * 1024
	rng := rand.New(rand.NewSource(0x50AC + seedN)) //nolint:gosec // deterministic fixture
	model := &soakModel{}

	seed := make([]byte, 6*1024*1024)
	rng.Read(seed)
	if _, err := bs.WriteAt(ctx, pid, nil, seed, 0); err != nil {
		t.Fatalf("seed WriteAt: %v", err)
	}
	model.write(0, seed)
	carve(t, bs, ctx, pid)

	for i := range iterations {
		label := fmt.Sprintf("iter-%d", i)
		_ = label
		switch rng.Intn(3) {
		case 0: // overwrite or extend
			off := uint64(rng.Intn(maxSize))
			length := 1 + rng.Intn(3*1024*1024)
			if int(off)+length > maxSize {
				length = maxSize - int(off)
			}
			data := make([]byte, length)
			rng.Read(data)
			if _, err := bs.WriteAt(ctx, pid, nil, data, off); err != nil {
				t.Fatalf("%s WriteAt(%d, %d): %v", label, off, length, err)
			}
			model.write(off, data)
			label = fmt.Sprintf("%s write(off=%d,len=%d)", label, off, length)
		case 1: // grow or shrink
			size := uint64(rng.Intn(maxSize))
			if _, err := bs.Truncate(ctx, pid, manifestRefs(t, ms, pid), size); err != nil {
				t.Fatalf("%s Truncate(%d): %v", label, size, err)
			}
			model.truncate(size)
			label = fmt.Sprintf("%s truncate(%d)", label, size)
		case 2: // punch
			if len(model.b) == 0 {
				continue
			}
			off := uint64(rng.Intn(len(model.b)))
			length := uint64(1 + rng.Intn(len(model.b)-int(off)))
			if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), off, length); err != nil {
				t.Fatalf("%s PunchHole(%d, %d): %v", label, off, length, err)
			}
			model.punch(off, length)
			label = fmt.Sprintf("%s punch(off=%d,len=%d)", label, off, length)
		}

		carve(t, bs, ctx, pid)
		ovOff, overlapped := overlappedOffset(t, ms, pid)

		// Force the file cold so the read resolves covering rows from the manifest
		// rather than serving warm local bytes. EvictLocal is a no-op.
		if _, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("%s DrainLocalSynced: %v", label, err)
		}
		if overlapped && ovOff < uint64(len(model.b)) {
			one := make([]byte, 1)
			_, perr := bs.ReadAt(ctx, pid, one, ovOff)
			switch {
			case perr != nil && errors.Is(perr, block.ErrManifestInconsistent):
				t.Fatalf("%s: READ BROKEN at double-covered offset %d: %v", label, ovOff, perr)
			case perr != nil:
				t.Fatalf("%s: read at double-covered offset %d: %v", label, ovOff, perr)
			case one[0] != model.b[ovOff]:
				t.Fatalf("%s: WRONG BYTE at double-covered offset %d", label, ovOff)
			default:
				t.Logf("%s: double-covered offset %d read fine", label, ovOff)
			}
		}
		if len(model.b) == 0 {
			continue
		}
		got := make([]byte, len(model.b))
		if _, err := bs.ReadAt(ctx, pid, got, 0); err != nil {
			if errors.Is(err, block.ErrManifestInconsistent) {
				t.Fatalf("%s: cold read reported ErrManifestInconsistent: %v", label, err)
			}
			t.Fatalf("%s cold ReadAt: %v", label, err)
		}
		t.Logf("%s ok (size=%d)", label, len(model.b))
		if !bytes.Equal(got, model.b) {
			first := 0
			for first < len(got) && got[first] == model.b[first] {
				first++
			}
			t.Fatalf("%s: cold read mismatch at byte %d (size %d)", label, first, len(model.b))
		}
	}
}
