package vfs

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// An owner's open of a file it already holds open, which widens that open,
// leaves it as it was when its answer fails: its deny mode still refuses
// another client, and its retried upgrade takes the next sequence.
func TestAFailedUpgradeKeepsTheOwnersOpen(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	g, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o666))
	n.user(t, "carol", "4321", n.share, entity.AccessReadWrite)
	c := n.nfs41(t, "v41", 4321, 1)
	owner := openstate.LockOwner{Client: c.Client, Name: []byte("open-owner")}
	first, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData, Deny: entity.WriteData, Owner: owner})
	n.must(t, err)
	n.engine.overlayErr = errors.New("journal read failed")
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Owner: owner}); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	if _, err := n.Open(ctx, n.smbClient(t, "pc", n.fs.Admin.Principal), vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.WriteData}); !errors.Is(err, vfs.ErrShareViolation) {
		t.Errorf("another client's write open after the failed upgrade: %v, want ErrShareViolation", err)
	}
	// The owner's retry is the upgrade its stateid's next sequence names,
	// as the failed one never reached it.
	up, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Owner: owner})
	n.must(t, err)
	if want := first.Ref.State.Seq + 1; up.Ref.State.Seq != want {
		t.Errorf("the retried upgrade's sequence: %d, want %d", up.Ref.State.Seq, want)
	}
	if _, err := n.Close(ctx, c, up.Ref, vfs.Seqs{}); err != nil {
		t.Errorf("the owner's close of its upgraded open: %v, want closed", err)
	}
}

// A delete-on-close open whose answer fails once open state recorded it
// leaves the file as a refused open does: not delete pending, and kept at
// the other open's close.
func TestAFailedDeleteOnCloseOpenMarksNothing(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	a := n.smbClient(t, "pc-a", n.fs.Admin.Principal)
	ra, err := n.Open(ctx, a, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData})
	n.must(t, err)
	doc := byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	n.engine.overlayErr = errors.New("journal read failed")
	if _, err := n.Open(ctx, n.smbClient(t, "pc-b", n.fs.Admin.Principal), doc); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	pc := n.smbClient(t, "pc-c", n.fs.Admin.Principal)
	if r, err := n.Open(ctx, pc, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}); err != nil {
		t.Errorf("an open after the failed one: %v, want granted", err)
	} else if _, err := n.Close(ctx, pc, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Close(ctx, a, ra.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil {
		t.Errorf("f after the other open's last close: %v, want kept", err)
	}
}
