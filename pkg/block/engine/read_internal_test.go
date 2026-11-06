package engine

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The memory of logged losses keeps the last lostLogMax extents: two past
// the bound forget the two oldest alone.
func TestTheLossLogIsBounded(t *testing.T) {
	var l lostLog
	f := entity.NewFileID()
	for i := range lostLogMax + 2 {
		if !l.first(lostKey{f, int64(i), 1}) {
			t.Fatalf("extent %d reported as already logged", i)
		}
	}
	if len(l.seen) != lostLogMax {
		t.Errorf("%d extents remembered; want %d", len(l.seen), lostLogMax)
	}
	for i, want := range []bool{false, false, true} {
		if _, ok := l.seen[lostKey{f, int64(i), 1}]; ok != want {
			t.Errorf("extent %d remembered %v; want %v", i, ok, want)
		}
	}
}
