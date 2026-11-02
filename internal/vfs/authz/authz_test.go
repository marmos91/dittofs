package authz

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/marmos91/dittofs/pkg/vfs"
)

func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}

// Each audit policy selects its decisions, and only decisions: an error
// that decides nothing emits nothing, and a write refused by a read-only
// narrowing is a decision.
func TestThePolicySelectsTheEvents(t *testing.T) {
	fs := kvstoretest.NewFS(t, memKV)
	admin := Caller{Identity: fs.Admin, Share: fs.Share.ID}
	nobody := Caller{Identity: entity.Identity{Principal: entity.NewUser()}, Share: fs.Share.ID}
	narrowed := admin
	narrowed.ReadOnly = entity.NarrowedByShare
	gone := entity.NewHandle(fs.Share.ID, entity.NewFileID())
	for _, c := range []struct {
		policy Policy
		caller Caller
		op     vfs.Op
		h      entity.Handle
		emits  bool
		want   vfs.Access
	}{
		{AuditOff, nobody, vfs.OpGetAttr, fs.Root, false, entity.ReadAttributes},
		{AuditDenials, nobody, vfs.OpGetAttr, fs.Root, true, entity.ReadAttributes},
		{AuditDenials, admin, vfs.OpSetAttr, fs.Root, false, entity.ReadAttributes},
		{AuditChanges, admin, vfs.OpSetAttr, fs.Root, true, entity.ReadAttributes},
		{AuditChanges, admin, vfs.OpGetAttr, fs.Root, false, entity.ReadAttributes},
		{AuditChanges, nobody, vfs.OpGetAttr, fs.Root, true, entity.ReadAttributes},
		{AuditAll, admin, vfs.OpGetAttr, fs.Root, true, entity.ReadAttributes},
		{AuditAll, admin, vfs.OpGetAttr, gone, false, entity.ReadAttributes},
		{AuditDenials, narrowed, vfs.OpSetAttr, fs.Root, true, entity.WriteAttributes},
	} {
		var got []vfs.AccessEvent
		a := Authz{
			Files:  fs.Store.Files(),
			Policy: func(vfs.ShareID) Policy { return c.policy },
			Emit:   func(_ context.Context, e vfs.AccessEvent) { got = append(got, e) },
		}
		err := a.Decide(t.Context(), c.caller, c.op, c.h, nil, nil, c.want)
		if len(got) != map[bool]int{true: 1}[c.emits] {
			t.Errorf("policy %d, op %v, allowed %v: %d events, want emitted %v", c.policy, c.op, err == nil, len(got), c.emits)
			continue
		}
		if c.emits && (got[0].Allowed != (err == nil) || got[0].Op != c.op || got[0].Share != fs.Share.ID) {
			t.Errorf("policy %d, op %v: event %+v for %v", c.policy, c.op, got[0], err)
		}
	}
}

// Decide and Rights hand the chokepoint the directory a file was resolved
// through, which grants delete on a file whose own mode grants none.
func TestViaReachesTheChokepoint(t *testing.T) {
	ctx := t.Context()
	fs := kvstoretest.NewFS(t, memKV)
	cp := fs.Store.ControlPlane()
	u := entity.User{Principal: entity.NewUser(), Name: "bob"}
	if err := cp.PutUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := cp.SetGrant(ctx, entity.ShareGrant{Share: fs.Share.ID, Principal: u.Principal, Access: entity.AccessReadWrite}); err != nil {
		t.Fatal(err)
	}
	bob := Caller{Identity: entity.Identity{Principal: u.Principal}, Share: fs.Share.ID}
	dir, _, err := fs.Store.Namespace().Create(ctx, fs.Admin, fs.Root, []byte("pub"), kvstore.Dir(0o777))
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := fs.Store.Namespace().Create(ctx, bob.Identity, dir, []byte("x"), kvstore.Regular(0o644))
	if err != nil {
		t.Fatal(err)
	}
	a := Authz{Files: fs.Store.Files()}
	if err := a.Decide(ctx, bob, vfs.OpOpen, doc, nil, nil, entity.Delete); !Refused(err) {
		t.Errorf("delete with no parent: %v", err)
	}
	if err := a.Decide(ctx, bob, vfs.OpOpen, doc, dir, nil, entity.Delete); err != nil {
		t.Errorf("delete through the writable parent: %v", err)
	}
	if got, err := a.Rights(ctx, bob, doc, dir, entity.Delete); err != nil || got != entity.Delete {
		t.Errorf("Rights through the writable parent: %#x, %v", got, err)
	}
}
