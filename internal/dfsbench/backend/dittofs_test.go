package backend

import (
	"fmt"
	"testing"
)

// TestDittofsDurabilityFlags_EachTierIsADistinctPair guards the matrix: nine
// registered backends differ only by metadata engine and tier, so two tiers
// expanding to the same pair would report cells that measured the same thing.
func TestDittofsDurabilityFlags_EachTierIsADistinctPair(t *testing.T) {
	seen := map[string]string{}
	for _, tier := range []string{"local", "writeback", "remote"} {
		ack, relaxed, err := dittofsDurabilityFlags(tier)
		if err != nil {
			t.Fatalf("dittofsDurabilityFlags(%q): %v", tier, err)
		}
		key := fmt.Sprintf("%s/%t", ack, relaxed)
		if prev, dup := seen[key]; dup {
			t.Errorf("tiers %q and %q both expand to %s", prev, tier, key)
		}
		seen[key] = tier
	}
}

// TestDittofsDurabilityFlags_RejectsAnUnknownTier keeps an unrecognised name
// from silently taking a default, which would label a run with a tier it never
// exercised.
func TestDittofsDurabilityFlags_RejectsAnUnknownTier(t *testing.T) {
	if _, _, err := dittofsDurabilityFlags("block-store"); err == nil {
		t.Fatal("got nil, want an error for a tier name that is not in the matrix")
	}
}
