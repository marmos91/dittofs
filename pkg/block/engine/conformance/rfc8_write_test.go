//go:build engine_conformance

package conformance

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// TestRFC8_WriteStagesTheClocksTime: a write's time reaches the journal as
// the clock's time in nanoseconds since the Unix epoch, and 0 when the write
// suspends the modification time, read back from the journal itself.
func TestRFC8_WriteStagesTheClocksTime(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("f")
	at := f.Clock.Now()
	write(t, f, id, 0, []byte("abc"))
	f.Clock.Advance(time.Minute)
	if _, err := f.E.Write(t.Context(), f.Share.ID, id, 3, []byte("def"), engine.WriteOpts{Done: func() {}, Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	var mtimes []int64
	for c, err := range f.Handle().Since(t.Context(), journal.NewFileID(id), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}
		mtimes = append(mtimes, c.Mtime)
	}
	if len(mtimes) != 2 || mtimes[0] != at.UnixNano() || mtimes[1] != 0 {
		t.Errorf("the journal's mtimes %v, want [%d 0]", mtimes, at.UnixNano())
	}
}
