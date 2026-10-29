//go:build journalv2_plant_split

package index_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
)

// TestFuzzFindsPlantedSplit runs the model-based fuzz over every seed and
// generated input on a build whose held set carries a known bug in its
// three-way split, and requires the fuzz to find it: the first seed, the
// three-way split itself, must diverge, and at least one input must overall.
// A driver bug must not make the test pass vacuously, so any misuse the
// model reports fails it here; every other test of the package is expected
// to fail on this build, which is why CI runs this test alone.
func TestFuzzFindsPlantedSplit(t *testing.T) {
	inputs := plantInputs()
	found := 0
	for i, in := range inputs {
		log, err := runModelFuzz(in)
		if errors.Is(err, model.ErrMisuse) {
			t.Fatalf("input %d hit a driver bug, not the planted one:\nsteps:\n%s\ngot: %v", i, strings.Join(log, "\n"), err)
		}
		// The first seed is checked before the passing skip, so a decoding or
		// driver change that stops it from reaching the split fails the test
		// rather than passing unheard behind the other inputs.
		if i == 0 {
			if !errors.Is(err, model.ErrDiverged) {
				t.Fatalf("the three-way-split seed returned %v, want the model to diverge; steps:\n%s", err, strings.Join(log, "\n"))
			}
			found++
			continue
		}
		if err == nil {
			continue
		}
		found++
	}
	if found == 0 {
		t.Fatal("no input found the planted split bug")
	}
	t.Logf("the fuzz found the planted split bug in %d of %d inputs", found, len(inputs))
}
