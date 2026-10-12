package kvstore

import (
	"context"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	db := memKV(t)
	if err := Init(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rows writes raw records, standing in for the control plane's writers.
func rows(t *testing.T, s *Store, kvs ...[]byte) {
	t.Helper()
	set := func(tx kv.Txn) error {
		for i := 0; i < len(kvs); i += 2 {
			if err := tx.Set(kvs[i], kvs[i+1]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.kv.Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
}

func member(g, m entity.Principal) [][]byte {
	return [][]byte{memberKey(g.ID, m.ID), encodeKind(m.Kind), memberRevKey(m.ID, g.ID), encodeKind(m.Kind)}
}

func grantRow(share entity.ShareID, p entity.Principal, a entity.ShareAccess) [][]byte {
	return [][]byte{grantKey(share, p.ID), encodeGrant(entity.ShareGrant{Principal: p, Access: a})}
}

func collectGroups(t *testing.T, s *Store, p entity.Principal) []entity.Principal {
	t.Helper()
	var out []entity.Principal
	for g, err := range s.Principals().Groups(context.Background(), p) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func TestGroupsTransitiveWithCycle(t *testing.T) {
	s := openStore(t)
	u, g1, g2, g3 := entity.NewUser(), entity.NewGroup(), entity.NewGroup(), entity.NewGroup()
	// u in g1, g1 in g2, g2 in g1 (a cycle), g3 unrelated.
	rows(t, s, slices.Concat(member(g1, u), member(g2, g1), member(g1, g2), member(g3, entity.NewUser()))...)
	got := collectGroups(t, s, u)
	if !slices.Equal(got, []entity.Principal{g1, g2}) {
		t.Fatalf("Groups(u) = %v, want [%v %v]", got, g1, g2)
	}
}

func TestGrantResolution(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := entity.NewShareID()
	u, v, g1, g2 := entity.NewUser(), entity.NewUser(), entity.NewGroup(), entity.NewGroup()
	rows(t, s, slices.Concat(
		member(g1, u), member(g2, g1), member(g1, v),
		grantRow(share, g1, entity.AccessRead),
		grantRow(share, g2, entity.AccessReadWrite), // reached through g1
		grantRow(share, v, entity.AccessNone),       // v's own grant decides
	)...)
	for _, c := range []struct {
		name string
		p    entity.Principal
		want entity.ShareAccess
	}{
		{"widest group grant", u, entity.AccessReadWrite},
		{"own grant of none wins over groups", v, entity.AccessNone},
		{"no grant", entity.NewUser(), entity.AccessNone},
	} {
		got, err := s.Principals().Grant(ctx, share, c.p)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if got, _ := s.Principals().Grant(ctx, entity.NewShareID(), u); got != entity.AccessNone {
		t.Errorf("other share: %v", got)
	}
}

func TestLookups(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, g := entity.NewUser(), entity.NewGroup()
	sidID := entity.ProtocolID{Scheme: entity.SchemeSID, Value: "\x01\x02"}
	rows(t, s,
		nameKey(keys.NXUser, "alice"), encodeID(u.ID),
		nameKey(keys.NXGroup, "alice"), encodeID(g.ID),
		nameKey(keys.NXGroup, "staff"), encodeID(g.ID),
		protocolKey(sidID), encodePrincipal(g),
	)
	p := s.Principals()
	if got, err := p.ByName(ctx, "alice"); err != nil || got != u {
		t.Errorf("ByName(alice) = %v, %v; want the user", got, err)
	}
	if got, err := p.ByName(ctx, "staff"); err != nil || got != g {
		t.Errorf("ByName(staff) = %v, %v", got, err)
	}
	if got, err := p.ByProtocolID(ctx, sidID); err != nil || got != g {
		t.Errorf("ByProtocolID(sid) = %v, %v", got, err)
	}
	if _, err := p.ByName(ctx, "nobody"); !merrs.IsNotFoundError(err) {
		t.Errorf("ByName(nobody): %v", err)
	}
	if _, err := p.ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeUID, Value: "7"}); !merrs.IsNotFoundError(err) {
		t.Errorf("ByProtocolID(uid 7): %v", err)
	}
}
