//go:build journal_conformance

package conformance

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// modelSteps is how many steps the seeded sequence of TestModel runs.
//
// ponytail: one sequence of 200 steps per run, comparing every file after
// every step, which keeps the run within a few seconds. Many seeds and longer
// sequences belong in the tiers that run for longer; raise this only if a
// failure needs a longer sequence to show and no corpus file holds it.
const modelSteps = 200

// modelSeed is the seed of TestModel's sequence.
//
// decision: the seed is DITTOFS_SEED when it is set, and a fixed one
// otherwise. A run that is retried inherits the same environment, so it draws
// the same sequence again and cannot pass by drawing another: a failing seed
// stays failing until the journal is fixed and its shrunk sequence committed.
// Only a tier that sets DITTOFS_SEED from a fresh draw explores new sequences.
func modelSeed(t *testing.T) uint64 {
	t.Helper()

	s, ok := os.LookupEnv("DITTOFS_SEED")
	if !ok {
		return 1
	}
	seed, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("DITTOFS_SEED=%q: %v", s, err)
	}
	return seed
}

// The journal agrees with its reference model after every step of random
// sequences of writes, syncs, reads, fills, offloads, releases, truncates,
// deallocates, deletes, settles, repacks, crashes of every kind the seam
// simulates, and clean reopens, over four files of two shares on one journal.
// Every sequence kept in testdata/model runs first, then the seeded one. A
// failing sequence is shrunk and written to the test's artifact directory, to
// be committed to testdata/model with the fix.
func TestModel(t *testing.T) {
	prof.Watch(t, prof.Options{})

	corpus, err := model.ReadCorpus("testdata/model")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	for _, seq := range corpus {
		model.Check(t, seq.Name, seq.Steps, journalv2test.ModelTargets(t), "kept as "+seq.Name)
	}

	seed := modelSeed(t)
	model.Check(t, fmt.Sprintf("seed-%d.txt", seed), model.Generate(seed, modelSteps), journalv2test.ModelTargets(t),
		fmt.Sprintf("drawn from seed %d, %d steps", seed, modelSteps))
}
