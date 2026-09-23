package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// failingExtentsLocal answers every DataExtents call with an error and
// delegates the rest of the interface to a real in-memory store, so only the
// one failure under test is simulated.
type failingExtentsLocal struct {
	journal.LocalStore
	err error
}

func (f failingExtentsLocal) DataExtents(context.Context, journal.FileID, int64) ([][2]uint64, error) {
	return nil, f.err
}

// TestDataExtents_LocalTierErrorWidensToWholeFile pins the direction a failure
// to measure the local tier resolves in.
//
// The tier holds exactly the bytes this map cannot source anywhere else —
// written but not yet rolled up into the CAS manifest — so an error leaves
// their ranges unknown rather than empty. Both callers drop the error and fall
// back to the CAS block list alone, the narrower view this function exists to
// widen, so returning the error puts a hole in the map over data that is really
// there and a sparse-copy client skips it.
//
// The whole-file answer is the same direction the unplaceable-row branch takes,
// and it is what the contract above requires: uncertainty resolves toward the
// answer that forces another question.
func TestDataExtents_LocalTierErrorWidensToWholeFile(t *testing.T) {
	ctx := context.Background()
	const fileSize = 8192

	bs := &Store{local: failingExtentsLocal{
		LocalStore: nil, // never reached: only DataExtents is called here
		err:        errors.New("local tier index unavailable"),
	}}

	got, err := bs.DataExtents(ctx, "payload-local-error", fileSize)
	if err != nil {
		t.Fatalf("DataExtents returned an error (%v); both callers drop it and answer from the "+
			"CAS block list alone, so the range reads as a hole over bytes the tier holds", err)
	}
	if want := [][2]uint64{{0, fileSize}}; !reflect.DeepEqual(got, want) {
		t.Errorf("DataExtents = %v, want %v — a tier that cannot report its ranges must widen "+
			"the map, not narrow it", got, want)
	}
}

// TestDataExtents_ClosedStoreStillErrors pins the deliberate exception. A
// closed store measured nothing rather than failing to place one range, and its
// caller's fallback is the best remaining source rather than the narrower one,
// so widening there would substitute a fabrication for a real answer.
func TestDataExtents_ClosedStoreStillErrors(t *testing.T) {
	bs := &Store{local: failingExtentsLocal{err: errors.New("unused")}}
	bs.closed = true

	if _, err := bs.DataExtents(context.Background(), "payload-closed", 8192); err == nil {
		t.Error("DataExtents on a closed store returned an answer; a store whose tiers are torn " +
			"down has no measurement to over-report")
	}
}
