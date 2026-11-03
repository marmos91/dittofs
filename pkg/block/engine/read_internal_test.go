package engine

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The memory of logged losses keeps the last lostLogMax extents: one past
// the bound forgets the oldest alone.
func TestTheLossLogIsBounded(t *testing.T) {
	var l lostLog
	f := entity.NewFileID()
	for i := range lostLogMax + 1 {
		if !l.first(lostKey{f, int64(i), 1}) {
			t.Fatalf("extent %d reported as already logged", i)
		}
	}
	if len(l.seen) != lostLogMax {
		t.Errorf("%d extents remembered; want %d", len(l.seen), lostLogMax)
	}
	if !l.first(lostKey{f, 0, 1}) {
		t.Error("the oldest extent is still remembered past the bound")
	}
	if l.first(lostKey{f, 2, 1}) {
		t.Error("an extent within the bound was forgotten")
	}
}
