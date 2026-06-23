package block

import (
	"reflect"
	"testing"
)

// ref builds a BlockRef at offset with the given size (hash left zero — the
// hole map only reads Offset/Size).
func ref(offset uint64, size uint32) BlockRef {
	return BlockRef{Offset: offset, Size: size}
}

func TestSegments(t *testing.T) {
	tests := []struct {
		name string
		refs []BlockRef
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
			refs: []BlockRef{ref(0, 100)},
			size: 100,
			want: []Segment{{SegmentData, 0, 100}},
		},
		{
			name: "leading hole then data",
			refs: []BlockRef{ref(50, 50)},
			size: 100,
			want: []Segment{{SegmentHole, 0, 50}, {SegmentData, 50, 100}},
		},
		{
			name: "data then trailing hole",
			refs: []BlockRef{ref(0, 40)},
			size: 100,
			want: []Segment{{SegmentData, 0, 40}, {SegmentHole, 40, 100}},
		},
		{
			name: "hole in the middle",
			refs: []BlockRef{ref(0, 20), ref(60, 40)},
			size: 100,
			want: []Segment{{SegmentData, 0, 20}, {SegmentHole, 20, 60}, {SegmentData, 60, 100}},
		},
		{
			name: "adjacent blocks merge into one data segment",
			refs: []BlockRef{ref(0, 20), ref(20, 20)},
			size: 40,
			want: []Segment{{SegmentData, 0, 40}},
		},
		{
			name: "block straddling EOF is clamped",
			refs: []BlockRef{ref(0, 200)},
			size: 100,
			want: []Segment{{SegmentData, 0, 100}},
		},
		{
			name: "unsorted overlapping input is normalized",
			refs: []BlockRef{ref(60, 40), ref(0, 70)},
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
	refs := []BlockRef{ref(0, 20), ref(60, 40)} // data [0,20) [60,100), hole [20,60)
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
	tailHole := []BlockRef{ref(0, 20)}
	if off, found := NextDataOffset(tailHole, 100, 50); found {
		t.Errorf("NextDataOffset in trailing hole = (%d,true), want not found", off)
	}
}

func TestNextHoleOffset(t *testing.T) {
	refs := []BlockRef{ref(0, 20), ref(60, 40)} // data [0,20) [60,100), hole [20,60)
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
	dense := []BlockRef{ref(0, 100)}
	if off, found := NextHoleOffset(dense, 100, 0); !found || off != 100 {
		t.Errorf("NextHoleOffset dense = (%d,%v), want (100,true)", off, found)
	}
}

func TestPunchHole(t *testing.T) {
	tests := []struct {
		name           string
		refs           []BlockRef
		offset, length uint64
		want           []BlockRef
	}{
		{
			name:   "zero length is no-op",
			refs:   []BlockRef{ref(0, 100)},
			offset: 10, length: 0,
			want: []BlockRef{ref(0, 100)},
		},
		{
			name:   "drop block fully inside range",
			refs:   []BlockRef{ref(0, 20), ref(40, 20), ref(80, 20)},
			offset: 40, length: 20,
			want: []BlockRef{ref(0, 20), ref(80, 20)},
		},
		{
			name:   "head-overlapping block is kept (out-of-range tail data survives)",
			refs:   []BlockRef{ref(0, 100)},
			offset: 50, length: 50,
			want: []BlockRef{ref(0, 100)},
		},
		{
			name:   "tail-overlapping block is kept (out-of-range head data survives)",
			refs:   []BlockRef{ref(50, 50)},
			offset: 0, length: 70,
			want: []BlockRef{ref(50, 50)},
		},
		{
			name:   "block straddling both ends is kept (head+tail data survive)",
			refs:   []BlockRef{ref(0, 100)},
			offset: 30, length: 40,
			want: []BlockRef{ref(0, 100)},
		},
		{
			name:   "no overlap leaves blocks untouched",
			refs:   []BlockRef{ref(0, 20), ref(80, 20)},
			offset: 40, length: 20,
			want: []BlockRef{ref(0, 20), ref(80, 20)},
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
