package shares

import (
	"errors"
	"testing"
)

// Startup must refuse a share whose recorded data location disagrees with the
// configured root rather than opening an empty journal beside it.
func TestStartupRefusesAMisplacedShare(t *testing.T) {
	err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{
		{Share: "/alpha", Path: "/mnt/elsewhere"},
	})
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("got %v, want a wrapped ErrJournalRootMismatch", err)
	}
}
