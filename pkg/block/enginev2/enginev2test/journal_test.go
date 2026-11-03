package enginev2test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// A refused Share or Reserve answers a nil interface, never one holding a nil
// pointer, which a caller's nil check would take for a handle.
func TestRefusalAnswersNil(t *testing.T) {
	h, err := wrapJournal(&journalv2.Journal{}).Share(1, 0, journalv2.Version{}, journalv2.Bounded)
	if !errors.Is(err, journalv2.ErrNotImplemented) || h != nil {
		t.Errorf("Share = %v, %v; want nil, the journal's error", h, err)
	}
	r, err := handle{&journalv2.Handle{}}.Reserve(1, 1)
	if !errors.Is(err, journalv2.ErrNotImplemented) || r != nil {
		t.Errorf("Reserve = %v, %v; want nil, the journal's error", r, err)
	}
}
