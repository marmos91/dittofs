package vfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/internal/vfs/pseudofs"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/sim"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// FuzzHandles: arbitrary handle bytes are refused before resolution, or
// admitted to their own share only; a pseudo handle names a node of the
// pseudo-filesystem or is refused. Share open admits the caller; share
// closed refuses its address, so any handle naming it is refused ErrAccess,
// a live file's or not.
func FuzzHandles(f *testing.F) {
	ctx := context.Background()
	db := memKV(f)
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		f.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		f.Fatal(err)
	}
	cp := st.ControlPlane()
	admin := entity.User{Principal: entity.NewUser(), Name: "admin", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "100"}}}
	open := entity.Share{ID: entity.NewShareID(), Name: "open", Path: "/a/open", Root: entity.NewFileID()}
	closed := entity.Share{ID: entity.NewShareID(), Name: "closed", Path: "/closed", Root: entity.NewFileID()}
	for _, err := range []error{
		cp.PutUser(ctx, admin), cp.PutShare(ctx, open), cp.PutShare(ctx, closed),
		cp.SetGrant(ctx, entity.ShareGrant{Share: open.ID, Principal: admin.Principal, Access: entity.AccessAdmin}),
		cp.SetGrant(ctx, entity.ShareGrant{Share: closed.ID, Principal: admin.Principal, Access: entity.AccessAdmin}),
		cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: closed.ID, Clients: []entity.ClientRule{
			{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("0.0.0.0/0")}, Access: entity.AccessNone}}}),
	} {
		if err != nil {
			f.Fatal(err)
		}
	}
	tab, err := openstate.New(openstate.Deps{Records: st, Clock: sim.NewTestClock(time.Unix(0, 0)), Rand: rand.Reader})
	if err != nil {
		f.Fatal(err)
	}
	s, err := New(Deps{Meta: st, Open: tab, Engine: newFakeContent()})
	if err != nil {
		f.Fatal(err)
	}
	if err := s.Start(ctx); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = s.Drain(ctx); _ = s.Stop() })
	id := proven(admin.Principal, inLab)
	tr, err := s.tree(ctx)
	if err != nil {
		f.Fatal(err)
	}
	openRoot := entity.NewHandle(open.ID, open.Root)
	for _, h := range [][]byte{
		openRoot, entity.NewHandle(closed.ID, closed.Root), entity.NewHandle(closed.ID, entity.NewFileID()),
		entity.NewHandle(open.ID, entity.NewFileID()),
		tr.Root().Handle(), tr.Root().Child("a").Handle(), nil, {1}, {pseudofs.Kind}, bytes.Repeat([]byte{pseudofs.Kind}, 33),
	} {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, h []byte) {
		info, err := s.StatFS(ctx, id, h)
		share, _, perr := entity.Handle(h).Parse()
		switch {
		case pseudofs.IsHandle(h):
			_, ok := s.pseudo.Load().Node(h)
			if ok != (err == nil) || !ok && !errors.Is(err, vfs.ErrStale) {
				t.Fatalf("pseudo handle %x: node %v, StatFS %v", h, ok, err)
			}
			if _, gerr := s.GetAttr(ctx, id, h); ok != (gerr == nil) {
				t.Fatalf("pseudo handle %x: node %v, GetAttr %v", h, ok, gerr)
			}
		case perr != nil:
			if !errors.Is(err, vfs.ErrStale) {
				t.Fatalf("malformed handle %x answered %v", h, err)
			}
		case share == closed.ID:
			if !errors.Is(err, vfs.ErrAccess) {
				t.Fatalf("handle %x into the refusing share answered %v", h, err)
			}
		case err == nil:
			if share != open.ID || info.Share != share || !bytes.Equal(h, openRoot) {
				t.Fatalf("handle %x admitted to %v, info for %v", h, share, info.Share)
			}
		case !errors.Is(err, vfs.ErrStale):
			t.Fatalf("handle %x answered %v", h, err)
		}
	})
}
