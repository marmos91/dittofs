package model

import (
	"testing"
)

// A journal that answers every call as the RFC allows, and keeps any state a
// crash allows, runs every generated sequence to the end: the runner drives
// the model only where it can follow.
func TestRunFollowsAnHonestJournal(t *testing.T) {
	for seed := range uint64(5) {
		steps := Generate(seed, 200)
		n, err := Run(t.Context(), newPlayed(t), steps)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if n != len(steps) {
			t.Fatalf("seed %d: ran %d of %d steps", seed, n, len(steps))
		}
	}
}
