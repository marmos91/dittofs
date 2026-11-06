package enginetest

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

// A refused Share or Reserve answers a nil interface, never one holding a nil
// pointer, which a caller's nil check would take for a handle.
func TestRefusalAnswersNil(t *testing.T) {
	h, err := wrapJournal(&journal.Journal{}, func(journal.ShareTag, engine.Handle) { t.Error("a refused Share handed a handle on") }).Share(1, 0, journal.Version{})
	if !errors.Is(err, journal.ErrNotImplemented) || h != nil {
		t.Errorf("Share = %v, %v; want nil, the journal's error", h, err)
	}
	r, err := handle{&journal.Handle{}}.Reserve(1, 1)
	if !errors.Is(err, journal.ErrNotImplemented) || r != nil {
		t.Errorf("Reserve = %v, %v; want nil, the journal's error", r, err)
	}
}
