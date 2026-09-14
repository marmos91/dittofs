package shares

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckJournalRoot_AcceptsMatchingPaths(t *testing.T) {
	err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{
		{Share: "a", Path: "/srv/blocks"},
		{Share: "b", Path: "/srv/blocks/"}, // trailing separator is the same place
	})
	if err != nil {
		t.Fatalf("CheckJournalRoot: got %v, want nil", err)
	}
}

func TestCheckJournalRoot_AcceptsNoRecordedPaths(t *testing.T) {
	if err := CheckJournalRoot("/srv/blocks", nil); err != nil {
		t.Errorf("fresh install: got %v, want nil", err)
	}
	if err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{{Share: "a"}}); err != nil {
		t.Errorf("share with no recorded path: got %v, want nil", err)
	}
}

// The caller decides refuse-vs-warn by matching the sentinel. If the returned
// error does not unwrap to it, a mismatched share is skipped with a warning and
// then mounted against the wrong directory, which reads back as zeros.
func TestCheckJournalRoot_ErrorUnwrapsToSentinel(t *testing.T) {
	err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{{Share: "a", Path: "/mnt/other"}})
	if err == nil {
		t.Fatal("got nil, want a mismatch error")
	}
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("errors.Is(err, ErrJournalRootMismatch) = false; err = %v", err)
	}
}

// One shared location can be adopted by pointing the config at it.
func TestCheckJournalRoot_SingleDivergentPathSuggestsAdoptingIt(t *testing.T) {
	err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{
		{Share: "a", Path: "/mnt/data"},
		{Share: "b", Path: "/mnt/data"},
	})
	if err == nil {
		t.Fatal("got nil, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "Set blockstore.journal.path to /mnt/data") {
		t.Errorf("error must tell the operator to adopt the shared path; got:\n%s", err)
	}
}

// Several locations cannot be expressed by one root, so the message must not
// suggest adopting one and stranding the rest.
func TestCheckJournalRoot_MultipleDivergentPathsDoNotSuggestOne(t *testing.T) {
	err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{
		{Share: "b", Path: "/mnt/two"},
		{Share: "a", Path: "/mnt/one"},
	})
	if err == nil {
		t.Fatal("got nil, want a mismatch error")
	}
	msg := err.Error()
	if strings.Contains(msg, "Set blockstore.journal.path to") {
		t.Errorf("must not suggest adopting one of several paths; got:\n%s", msg)
	}
	for _, want := range []string{`share "a"`, "/mnt/one", `share "b"`, "/mnt/two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name %q; got:\n%s", want, msg)
		}
	}
	// Named in a stable order so the message does not churn between restarts.
	if strings.Index(msg, `share "a"`) > strings.Index(msg, `share "b"`) {
		t.Errorf("shares must be named in sorted order; got:\n%s", msg)
	}
}

func TestCheckJournalRoot_RefusesWhenNoRootConfigured(t *testing.T) {
	err := CheckJournalRoot("", []ShareJournalPath{{Share: "a", Path: "/mnt/data"}})
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("got %v, want a wrapped ErrJournalRootMismatch", err)
	}
}
