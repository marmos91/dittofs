//go:build engine_conformance

package conformance

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// TestRFC8_FixtureRecordsItsJournal: the fixture records its journal's
// identity for the node and directory its engine runs with, so the store's
// Lifecycle view answers the engine's start with that identity.
func TestRFC8_FixtureRecordsItsJournal(t *testing.T) {
	f := enginetest.New(t)

	got, err := f.FS.Store.Lifecycle().JournalID(t.Context(), f.Node, entity.JournalDir(f.Dir))
	if err != nil {
		t.Fatal(err)
	}

	if got != f.Journal {
		t.Errorf(
			"JournalID of the fixture's directory: %x, want the fixture's journal %x",
			got,
			f.Journal,
		)
	}
}
