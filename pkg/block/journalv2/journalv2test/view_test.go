package journalv2test

import (
	"errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/sim"
)

func TestAStaleViewTouchesNothing(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "aaaa")

	v := s.FS()
	crash(t, s)
	before := s.Calls(OpCreate) + s.Calls(OpOpen) + s.Calls(OpRename) + s.Calls(OpRemove) + s.Calls(OpList) + s.Calls(OpSyncDir)

	calls := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { _, err := v.Create("b"); return err }},
		{"Open", func() error { _, err := v.Open("a"); return err }},
		{"Rename", func() error { return v.Rename("a", "c") }},
		{"Remove", func() error { return v.Remove("a") }},
		{"List", func() error { _, err := v.List(); return err }},
		{"SyncDir", v.SyncDir},
	}
	for _, c := range calls {
		if err := c.call(); !errors.Is(err, fs.ErrClosed) {
			t.Errorf("%s through a view from before the crash returned %v, want fs.ErrClosed", c.name, err)
		}
	}

	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"a"}) {
		t.Fatalf("the directory holds %q after the stale calls, want only a", names)
	}
	wantDurable(t, s, "a", new("aaaa"))

	after := s.Calls(OpCreate) + s.Calls(OpOpen) + s.Calls(OpRename) + s.Calls(OpRemove) + s.Calls(OpList) + s.Calls(OpSyncDir)
	if after != before+1 { // the List above
		t.Fatalf("the stale calls were counted: %d calls, want %d", after-before-1, 0)
	}
}

func TestAViewMadeAfterACrashServesTheDirectory(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "aaaa")
	crash(t, s)

	v := s.FS()
	f, err := v.Open("a")
	if err != nil {
		t.Fatalf("Open through a view made after the crash: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	g, err := v.Create("b")
	if err != nil {
		t.Fatalf("Create through a view made after the crash: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAHandleIsKeyedByTheNameItsFileHasNow(t *testing.T) {
	s := newSeam(t)
	f, err := s.Create("1.spare.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("1.spare.tmp", "1.seg"); err != nil {
		t.Fatal(err)
	}

	h := s.Hold(t, OpSync, "*.seg")
	done := goSync(f)
	within(t, h.Reached(), "a sync through a handle opened before the rename reaching a hold on the new name")
	h.Release()
	if err := within(t, done, "the released sync returning"); err != nil {
		t.Fatal(err)
	}

	if got := s.CallsFor(OpSync, "1.seg"); got != 1 {
		t.Fatalf("the sync was counted %d times under the file's new name, want 1", got)
	}
	if got := f.Name(); got != "1.spare.tmp" {
		t.Fatalf("Name() = %q, want the name the file was opened under", got)
	}
}

func TestAFaultMatchesARenamedFileByItsNewName(t *testing.T) {
	s := newSeam(t)
	f, err := s.Create("1.spare.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("1.spare.tmp", "1.seg"); err != nil {
		t.Fatal(err)
	}

	eio := errors.New("injected")
	fault, err := sim.NewSchedule(1, nil, sim.Rule{Op: OpSync, Key: "*.seg", Action: sim.FaultAction{Err: eio}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(fault)

	if err := f.Sync(); !errors.Is(err, eio) {
		t.Fatalf("a sync through a handle opened before the rename returned %v, want the fault on the new name", err)
	}
}

func TestARemovedFileAnswersToTheNameItWasOpenedUnder(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "a")
	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}

	h := s.Hold(t, OpSync, "a")
	done := goSync(f)
	within(t, h.Reached(), "a sync of a removed file reaching a hold on the name it was opened under")
	h.Release()
	if err := within(t, done, "the released sync returning"); err != nil {
		t.Fatal(err)
	}
}
