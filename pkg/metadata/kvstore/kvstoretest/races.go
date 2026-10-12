package kvstoretest

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// holding runs a hook once, inside the next Update, after its function has
// done all its reads and writes and before it commits. A hook that runs a
// second operation to completion makes that one commit between the first's
// snapshot and its commit: the order in which the first must conflict.
type holding struct {
	kv.KV
	mu   sync.Mutex
	hook func()
}

func (h *holding) Update(ctx context.Context, fn func(kv.Txn) error) error {
	h.mu.Lock()
	hook := h.hook
	h.hook = nil
	h.mu.Unlock()
	return h.KV.Update(ctx, func(tx kv.Txn) error {
		err := fn(tx)
		if hook != nil {
			hook()
			hook = nil
		}
		return err
	})
}

// backend is the KV a race case runs on, named for its subtest.
type backend struct {
	name string
	open Opener
}

func heldStore(t *testing.T, b backend) (*kvstore.Store, *holding) {
	t.Helper()
	h := &holding{KV: b.open(t)}
	must(t, kvstore.Init(ctx, h))
	s, err := kvstore.Open(ctx, h)
	must(t, err)
	return s, h
}

// race runs outer with inner committing inside outer's first attempt, and
// returns both results.
func race(h *holding, outer, inner func() error) (outerErr, innerErr error) {
	h.mu.Lock()
	h.hook = func() { innerErr = inner() }
	h.mu.Unlock()
	return outer(), innerErr
}

// Two writers claim one unique row: whichever commits second is refused,
// in both orders.
func testUniqueClaimsRace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			for _, c := range []struct {
				name string
				a, b func(cp kvstore.ControlPlane) error
			}{
				{"user name",
					func(cp kvstore.ControlPlane) error { return cp.PutUser(ctx, user("alice")) },
					func(cp kvstore.ControlPlane) error { return cp.PutUser(ctx, user("alice")) }},
				{"uid",
					func(cp kvstore.ControlPlane) error { return cp.PutUser(ctx, user("a", uid("7"))) },
					func(cp kvstore.ControlPlane) error { return cp.PutUser(ctx, user("b", uid("7"))) }},
				{"share name under the fold",
					func(cp kvstore.ControlPlane) error { return cp.PutShare(ctx, share("Photos", "/a")) },
					func(cp kvstore.ControlPlane) error { return cp.PutShare(ctx, share("PHOTOS", "/b")) }},
				{"nested share paths",
					func(cp kvstore.ControlPlane) error { return cp.PutShare(ctx, share("home", "/home")) },
					func(cp kvstore.ControlPlane) error { return cp.PutShare(ctx, share("alice", "/home/alice")) }},
			} {
				for _, order := range []string{"a held", "b held"} {
					t.Run(c.name+"/"+order, func(t *testing.T) {
						s, h := heldStore(t, b)
						cp := s.ControlPlane()
						first, second := c.a, c.b
						if order == "b held" {
							first, second = c.b, c.a
						}
						outer, inner := race(h, func() error { return first(cp) }, func() error { return second(cp) })
						if inner != nil {
							t.Fatalf("the operation committing first failed: %v", inner)
						}
						if !isExist(outer) {
							t.Fatalf("the operation committing second: %v, want already exists", outer)
						}
					})
				}
			}
		})
	}
}

// A membership or grant added while its principal is deleted never
// outlives the principal, in either order.
func testDeleteRacesAdd(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			for _, c := range []struct {
				name string
				add  func(cp kvstore.ControlPlane, u, g entity.Principal, sh entity.ShareID) error
			}{
				{"membership", func(cp kvstore.ControlPlane, u, g entity.Principal, _ entity.ShareID) error {
					return cp.AddMember(ctx, entity.Membership{Group: g, Member: u})
				}},
				{"grant", func(cp kvstore.ControlPlane, u, _ entity.Principal, sh entity.ShareID) error {
					return cp.SetGrant(ctx, entity.ShareGrant{Share: sh, Principal: u, Access: entity.AccessRead})
				}},
			} {
				for _, deleteHeld := range []bool{true, false} {
					name := c.name + "/add commits first"
					if !deleteHeld {
						name = c.name + "/delete commits first"
					}
					t.Run(name, func(t *testing.T) {
						s, h := heldStore(t, b)
						cp, p := s.ControlPlane(), s.Principals()
						u, g, sh := user("alice"), group("staff"), share("photos", "/photos")
						must(t, cp.PutUser(ctx, u))
						must(t, cp.PutGroup(ctx, g))
						must(t, cp.PutShare(ctx, sh))
						del := func() error { return cp.DeleteUser(ctx, u.Principal) }
						add := func() error { return c.add(cp, u.Principal, g.Principal, sh.ID) }
						var delErr, addErr error
						if deleteHeld {
							delErr, addErr = race(h, del, add)
						} else {
							addErr, delErr = race(h, add, del)
						}
						must(t, delErr)
						if !deleteHeld && !merrs.IsNotFoundError(addErr) {
							t.Errorf("add after the delete: %v, want not found", addErr)
						}
						if got := all(t, p.Groups(ctx, u.Principal)); len(got) != 0 {
							t.Errorf("deleted user still in %v", got)
						}
						if a, err := p.Grant(ctx, sh.ID, u.Principal); err != nil || a != entity.AccessNone {
							t.Errorf("deleted user's grant: %v, %v", a, err)
						}
					})
				}
			}
		})
	}
}

// Of two state changes from one state, the second to commit is refused.
func testShareStateRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	cp := s.ControlPlane()
	sh := share("photos", "/photos")
	must(t, cp.PutShare(ctx, sh))
	outer, inner := race(h,
		func() error { return cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareFrozen) },
		func() error { return cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareQuiesced) })
	must(t, inner)
	if !merrs.IsConflictError(outer) {
		t.Fatalf("second change: %v", outer)
	}
}
