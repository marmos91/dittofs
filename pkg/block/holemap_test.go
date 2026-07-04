package block

import (
	"reflect"
	"testing"
)

// ref builds a ChunkRef at offset with the given size (hash left zero — the
// hole map only reads Offset/Size).
func ref(offset uint64, size uint32) ChunkRef {
	return ChunkRef{Offset: offset, Size: size}
}

func TestSegments(t *testing.T) {
	tests := []struct {
		name string
		refs []ChunkRef
		size uint64
		want []Segment
	}{
		{
			name: "empty file",
			size: 0,
			want: nil,
		},
		{
			name: "no blocks is all hole",
			size: 100,
			want: []Segment{{SegmentHole, 0, 100}},
		},
		{
			name: "fully covered is single data segment",
			refs: []ChunkRef{ref(0, 100)},
			size: 100,
			want: []Segment{{SegmentData, 0, 100}},
		},
		{
			name: "leading hole then data",
			refs: []ChunkRef{ref(50, 50)},
			size: 100,
			want: []Segment{{SegmentHole, 0, 50}, {SegmentData, 50, 100}},
		},
		{
			name: "data then trailing hole",
			refs: []ChunkRef{ref(0, 40)},
			size: 100,
			want: []Segment{{SegmentData, 0, 40}, {SegmentHole, 40, 100}},
		},
		{
			name: "hole in the middle",
			refs: []ChunkRef{ref(0, 20), ref(60, 40)},
			size: 100,
			want: []Segment{{SegmentData, 0, 20}, {SegmentHole, 20, 60}, {SegmentData, 60, 100}},
		},
		{
			name: "adjacent blocks merge into one data segment",
			refs: []ChunkRef{ref(0, 20), ref(20, 20)},
			size: 40,
			want: []Segment{{SegmentData, 0, 40}},
		},
		{
			name: "block straddling EOF is clamped",
			refs: []ChunkRef{ref(0, 200)},
			size: 100,
			want: []Segment{{SegmentData, 0, 100}},
		},
		{
			name: "unsorted overlapping input is normalized",
			refs: []ChunkRef{ref(60, 40), ref(0, 70)},
			size: 100,
			want: []Segment{{SegmentData, 0, 100}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Segments(tt.refs, tt.size)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Segments() = %+v, want %+v", got, tt.want)
			}
			// Invariant: segments tile [0,size) with no gaps/overlaps.
			var pos uint64
			for _, s := range got {
				if s.Start != pos {
					t.Fatalf("segment gap/overlap: start %d, want %d", s.Start, pos)
				}
				if s.End <= s.Start {
					t.Fatalf("empty/negative segment %+v", s)
				}
				pos = s.End
			}
			if pos != tt.size {
				t.Fatalf("segments cover [0,%d), want [0,%d)", pos, tt.size)
			}
		})
	}
}

func TestNextDataOffset(t *testing.T) {
	refs := []ChunkRef{ref(0, 20), ref(60, 40)} // data [0,20) [60,100), hole [20,60)
	size := uint64(100)
	tests := []struct {
		from      uint64
		wantOff   uint64
		wantFound bool
	}{
		{0, 0, true},    // inside first data extent
		{10, 10, true},  // inside first data extent
		{20, 60, true},  // start of hole -> next data at 60
		{40, 60, true},  // inside hole -> next data at 60
		{60, 60, true},  // inside second data extent
		{99, 99, true},  // last data byte
		{100, 0, false}, // at EOF -> NXIO
		{200, 0, false}, // past EOF -> NXIO
	}
	for _, tt := range tests {
		off, found := NextDataOffset(refs, size, tt.from)
		if off != tt.wantOff || found != tt.wantFound {
			t.Errorf("NextDataOffset(from=%d) = (%d,%v), want (%d,%v)", tt.from, off, found, tt.wantOff, tt.wantFound)
		}
	}

	// A file whose tail is a hole: SEEK_DATA past the last data returns NXIO.
	tailHole := []ChunkRef{ref(0, 20)}
	if off, found := NextDataOffset(tailHole, 100, 50); found {
		t.Errorf("NextDataOffset in trailing hole = (%d,true), want not found", off)
	}
}

func TestNextHoleOffset(t *testing.T) {
	refs := []ChunkRef{ref(0, 20), ref(60, 40)} // data [0,20) [60,100), hole [20,60)
	size := uint64(100)
	tests := []struct {
		from      uint64
		wantOff   uint64
		wantFound bool
	}{
		{0, 20, true},   // inside data -> hole begins at 20
		{19, 20, true},  // last data byte before hole
		{20, 20, true},  // inside hole already
		{40, 40, true},  // inside hole already
		{60, 100, true}, // inside trailing data -> virtual hole at EOF
		{99, 100, true}, // last data byte -> hole at EOF
		{100, 0, false}, // at EOF -> NXIO
	}
	for _, tt := range tests {
		off, found := NextHoleOffset(refs, size, tt.from)
		if off != tt.wantOff || found != tt.wantFound {
			t.Errorf("NextHoleOffset(from=%d) = (%d,%v), want (%d,%v)", tt.from, off, found, tt.wantOff, tt.wantFound)
		}
	}

	// Fully dense file: the only hole is the virtual one at EOF.
	dense := []ChunkRef{ref(0, 100)}
	if off, found := NextHoleOffset(dense, 100, 0); !found || off != 100 {
		t.Errorf("NextHoleOffset dense = (%d,%v), want (100,true)", off, found)
	}
}

// TestNextOffsetExtents exercises the extent-based SEEK primitives directly on a
// pre-computed data view (the form engine.DataExtents returns) — the path SEEK
// takes for the un-rolled-up data that the CAS block list omits (#1481).
func TestNextOffsetExtents(t *testing.T) {
	// Sparse layout: data [0,20), hole [20,60), data [60,100). Same shape as
	// the ChunkRef-based tests so the extent variants are verified to agree.
	extents := [][2]uint64{{0, 20}, {60, 100}}
	size := uint64(100)

	// SEEK_DATA from inside the gap finds the second data region.
	if off, found := NextDataOffsetExtents(extents, size, 40); !found || off != 60 {
		t.Errorf("NextDataOffsetExtents(from=40) = (%d,%v), want (60,true)", off, found)
	}
	// SEEK_DATA inside the first data region stays put.
	if off, found := NextDataOffsetExtents(extents, size, 10); !found || off != 10 {
		t.Errorf("NextDataOffsetExtents(from=10) = (%d,%v), want (10,true)", off, found)
	}
	// SEEK_HOLE from inside the first data region finds the interior hole.
	if off, found := NextHoleOffsetExtents(extents, size, 10); !found || off != 20 {
		t.Errorf("NextHoleOffsetExtents(from=10) = (%d,%v), want (20,true)", off, found)
	}
	// SEEK_HOLE from inside the trailing data region finds the EOF hole.
	if off, found := NextHoleOffsetExtents(extents, size, 80); !found || off != 100 {
		t.Errorf("NextHoleOffsetExtents(from=80) = (%d,%v), want (100,true)", off, found)
	}
	// At/after EOF: NXIO for both.
	if _, found := NextDataOffsetExtents(extents, size, 100); found {
		t.Error("NextDataOffsetExtents at EOF = found, want not found")
	}
	if _, found := NextHoleOffsetExtents(extents, size, 100); found {
		t.Error("NextHoleOffsetExtents at EOF = found, want not found")
	}

	// The extent variants must agree with the ChunkRef wrappers that delegate
	// to them (regression guard for the refactor).
	refs := []ChunkRef{ref(0, 20), ref(60, 40)}
	for from := uint64(0); from <= size; from++ {
		dOff, dFound := NextDataOffset(refs, size, from)
		eOff, eFound := NextDataOffsetExtents(extents, size, from)
		if dOff != eOff || dFound != eFound {
			t.Errorf("data disagree at from=%d: refs=(%d,%v) extents=(%d,%v)", from, dOff, dFound, eOff, eFound)
		}
		hOff, hFound := NextHoleOffset(refs, size, from)
		xOff, xFound := NextHoleOffsetExtents(extents, size, from)
		if hOff != xOff || hFound != xFound {
			t.Errorf("hole disagree at from=%d: refs=(%d,%v) extents=(%d,%v)", from, hOff, hFound, xOff, xFound)
		}
	}
}

func TestPunchHole(t *testing.T) {
	tests := []struct {
		name           string
		refs           []ChunkRef
		offset, length uint64
		want           []ChunkRef
	}{
		{
			name:   "zero length is no-op",
			refs:   []ChunkRef{ref(0, 100)},
			offset: 10, length: 0,
			want: []ChunkRef{ref(0, 100)},
		},
		{
			name:   "drop block fully inside range",
			refs:   []ChunkRef{ref(0, 20), ref(40, 20), ref(80, 20)},
			offset: 40, length: 20,
			want: []ChunkRef{ref(0, 20), ref(80, 20)},
		},
		{
			name:   "head-overlapping block is kept (out-of-range tail data survives)",
			refs:   []ChunkRef{ref(0, 100)},
			offset: 50, length: 50,
			want: []ChunkRef{ref(0, 100)},
		},
		{
			name:   "tail-overlapping block is kept (out-of-range head data survives)",
			refs:   []ChunkRef{ref(50, 50)},
			offset: 0, length: 70,
			want: []ChunkRef{ref(50, 50)},
		},
		{
			name:   "block straddling both ends is kept (head+tail data survive)",
			refs:   []ChunkRef{ref(0, 100)},
			offset: 30, length: 40,
			want: []ChunkRef{ref(0, 100)},
		},
		{
			name:   "no overlap leaves blocks untouched",
			refs:   []ChunkRef{ref(0, 20), ref(80, 20)},
			offset: 40, length: 20,
			want: []ChunkRef{ref(0, 20), ref(80, 20)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PunchHole(tt.refs, tt.offset, tt.length)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("PunchHole() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
