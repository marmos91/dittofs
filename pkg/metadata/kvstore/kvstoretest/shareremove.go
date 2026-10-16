package kvstoretest

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

func testDeleteShareMark(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	cp := f.s.ControlPlane()
	before, _ := cp.ShareList(ctx)
	must(t, cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsRefuse))

	if after, _ := cp.ShareList(ctx); after.Version != before.Version+1 {
		t.Errorf("ShareList %d -> %d", before.Version, after.Version)
	}
	if got := all(t, cp.Shares(ctx)); len(got) != 0 {
		t.Errorf("a share being removed is listed: %+v", got)
	}
	// The name and path are free at once.
	must(t, cp.PutShare(ctx, share("data", "/data")))

	// Removal is one-way, and the share is stale to every caller.
	for name, err := range map[string]error{
		"second delete": cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsDelete),
		"state change":  cp.SetShareState(ctx, f.share.ID, entity.ShareRemoving, entity.ShareEnabled),
		"rename":        cp.PutShare(ctx, f.share),
		"grant":         cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: f.admin.Principal, Access: entity.AccessRead}),
		"policy":        cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: f.share.ID}),
		"root":          errOf2(f.s.Files().Root(ctx, f.admin, f.share.ID)),
	} {
		if !errors.Is(err, entity.ErrStale) {
			t.Errorf("%s: %v, want stale", name, err)
		}
	}
	if err := cp.DeleteShare(ctx, entity.NewShareID(), entity.SnapshotsRefuse); !merrs.IsNotFoundError(err) {
		t.Errorf("deleting a missing share: %v", err)
	}
	if err := cp.SetShareState(ctx, f.share.ID, entity.ShareEnabled, entity.ShareRemoving); err == nil {
		t.Error("SetShareState started a removal")
	}
}

func errOf2(_ entity.Handle, _ entity.File, err error) error { return err }

// The mark and a rename of the same share, in both commit orders: whichever
// commits second sees the other, so a removed share is never renamed back
// into the index.
func testDeleteShareRacesRename(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for _, deleteHeld := range []bool{true, false} {
			name := b.name + "/rename commits first"
			if !deleteHeld {
				name = b.name + "/delete commits first"
			}
			t.Run(name, func(t *testing.T) {
				f, h := newHeldFS(t, b, entity.FoldIdentity)
				cp := f.s.ControlPlane()
				renamed := f.share
				renamed.Name = "renamed"
				del := func() error { return cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsRefuse) }
				ren := func() error { return cp.PutShare(ctx, renamed) }
				var delErr, renErr error
				if deleteHeld {
					delErr, renErr = race(h, del, ren)
					must(t, renErr)
					must(t, delErr)
				} else {
					renErr, delErr = race(h, ren, del)
					must(t, delErr)
					if !errors.Is(renErr, entity.ErrStale) {
						t.Fatalf("rename after the removal: %v", renErr)
					}
				}
				if got := all(t, cp.Shares(ctx)); len(got) != 0 {
					t.Fatalf("index after both: %+v", got)
				}
				// Both names are free.
				must(t, cp.PutShare(ctx, share("data", "/x")))
				must(t, cp.PutShare(ctx, share("renamed", "/y")))
			})
		}
	}
}
