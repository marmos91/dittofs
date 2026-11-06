package durability_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestGenerationOutlivesADurabilityOnTheSameDirectory catches a reopen in one
// process that restarts the generation at zero, which would hand a client a
// verifier it has already seen.
func TestGenerationOutlivesADurabilityOnTheSameDirectory(t *testing.T) {
	e := newEnv(t)

	for range 3 {
		e.d.RaiseWindow()
	}
	if got := e.d.Generation(); got != 3 {
		t.Fatalf("generation = %d, want 3", got)
	}

	reopened := e.newDurability()
	if got := reopened.Generation(); got < 3 {
		t.Fatalf("the reopened generation = %d, want at least 3", got)
	}
	if _, err := reopened.Lose(context.Background(), loss(1)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if got := reopened.Generation(); got != 4 {
		t.Errorf("generation after one dirty corrupt loss on the reopen = %d, want 4", got)
	}
	if got := e.d.Generation(); got != 4 {
		t.Errorf("the first Durability reads %d, want the directory's 4", got)
	}
}

// TestGenerationOfAnotherDirectoryStartsAtZero catches a registry keyed on
// nothing, which would share one count across every journal in the process.
func TestGenerationOfAnotherDirectoryStartsAtZero(t *testing.T) {
	prof.Watch(t, prof.Options{})
	a := buildEnv(t)
	b := buildEnv(t)

	a.d.RaiseWindow()
	if got := b.d.Generation(); got != 0 {
		t.Errorf("a second directory's generation = %d, want 0", got)
	}
}

// TestGenerationIsKeyedByTheCleanedAbsolutePath catches two spellings of one
// directory counting separately.
func TestGenerationIsKeyedByTheCleanedAbsolutePath(t *testing.T) {
	e := newEnv(t)
	e.d.RaiseWindow()

	other := e.newDurabilityAt(e.dir + "/sub/..")
	if got := other.Generation(); got != 1 {
		t.Errorf("another spelling of the directory reads %d, want 1", got)
	}
}
