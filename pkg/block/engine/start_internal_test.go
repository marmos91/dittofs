package engine

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Every share record of the installation counts in the floor the journal
// opens with, a share being removed included, and so does a share handed to
// Start that no record names yet (RFC 8 §2.5 step 1). Leaving the removed
// ones out opens the journal below versions their files already hold.
func TestTheOpenFloorCountsEveryShare(t *testing.T) {
	serving := entity.NewShareID()
	offloaded := entity.NewShareID()
	forgotten := entity.NewShareID()
	handed := entity.NewShareID()

	tags := map[entity.ShareID]entity.TagRecord{
		serving: {
			Tag:   1,
			State: entity.TagServing,
		},
		offloaded: {
			Tag:   2,
			State: entity.TagOffloaded,
		},
		forgotten: {
			Tag:   3,
			State: entity.TagForgotten,
		},
	}
	shares := []Share{
		{ID: serving, Tag: 1},
		{ID: handed, Tag: 4},
	}

	got := floorShares(shares, tags)

	want := []entity.ShareID{serving, offloaded, forgotten, handed}

	for _, id := range want {
		if !slices.Contains(got, id) {
			t.Errorf("the floor's shares %v leave out %v", got, id)
		}
	}

	if len(got) != len(want) {
		t.Errorf("the floor's shares are %d, want %d, each once", len(got), len(want))
	}
}

// The floor the journal opens with is the largest of the shares' version
// floor and every share's version limit (RFC 8 §2.5 step 1, §2.7; RFC 28
// §3.1). The smallest, or the floor or the limits alone, reissue after a
// crash a version a client already saw.
func TestTheOpenFloorIsTheLargestOfAll(t *testing.T) {
	a := entity.NewShareID()
	b := entity.NewShareID()

	for _, c := range []struct {
		name   string
		floor  uint64
		limits map[entity.ShareID]entity.JournalVersion
		want   uint64
	}{
		{
			name:   "another share's limit is the largest",
			floor:  5,
			limits: map[entity.ShareID]entity.JournalVersion{a: entity.VersionOf(7), b: entity.VersionOf(9)},
			want:   9,
		},
		{
			name:   "the floor is above every limit",
			floor:  12,
			limits: map[entity.ShareID]entity.JournalVersion{a: entity.VersionOf(7), b: entity.VersionOf(9)},
			want:   12,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := openFloor(entity.VersionOf(c.floor), c.limits)

			if got != entity.VersionOf(c.want) {
				t.Errorf("the open floor is %d, want %d", got.Counter(), c.want)
			}
		})
	}
}

// Start writes every served share's version limit one block above the
// journal's next version, with a zero epoch (RFC 8 §2.7, RFC 1 §4.3). With no
// block the journal issues nothing, and with the epoch copied the next open
// fails.
func TestTheStartLimitIsABlockAboveTheNextVersion(t *testing.T) {
	got := startLimit(journal.Version{Epoch: 3, Counter: 5}, versionBlock)

	want := journal.Version{Counter: 5 + versionBlock}

	if got != want {
		t.Errorf("the start limit is %+v, want %+v", got, want)
	}
}
