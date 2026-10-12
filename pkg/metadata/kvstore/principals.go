package kvstore

import (
	"context"
	"errors"
	"iter"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Principals is what authentication and the authorisation chokepoint read.
type Principals struct{ s *Store }

func (s *Store) Principals() Principals { return Principals{s} }

// ByName finds a user by name, or failing that a group.
//
// decision: a user and a group may share a name, as a user's private group
// does on POSIX systems, and the name lookup takes no kind, so the user wins.
// A caller that means the group cannot reach it through this call; give the
// lookup a kind if one ever needs to.
func (p Principals) ByName(ctx context.Context, name string) (entity.Principal, error) {
	var out entity.Principal
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		for _, kind := range []entity.PrincipalKind{entity.PrincipalUser, entity.PrincipalGroup} {
			b, err := r.Get(nameKey(nameKind(kind), name))
			if errors.Is(err, kv.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			id, err := decodeID(b)
			out = entity.Principal{Kind: kind, ID: id}
			return err
		}
		return merrs.NewNotFoundError(name, "principal")
	})
	return out, err
}

func (p Principals) ByProtocolID(ctx context.Context, id entity.ProtocolID) (entity.Principal, error) {
	var out entity.Principal
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(protocolKey(id))
		if errors.Is(err, kv.ErrNotFound) {
			return merrs.NewNotFoundError(id.Scheme.String()+":"+id.Value, "principal")
		}
		if err != nil {
			return err
		}
		out, err = decodePrincipal(b)
		return err
	})
	return out, err
}

// Groups yields every group p is a member of, directly or through nested
// groups, each once, nearest first. A cycle of nested groups ends the walk
// rather than loop.
func (p Principals) Groups(ctx context.Context, of entity.Principal) iter.Seq2[entity.Principal, error] {
	return func(yield func(entity.Principal, error) bool) {
		var groups []entity.Principal
		err := p.s.kv.View(ctx, func(r kv.Reader) (err error) {
			groups, err = groupsOf(r, of)
			return err
		})
		if err != nil {
			yield(entity.Principal{}, err)
			return
		}
		for _, g := range groups {
			if !yield(g, nil) {
				return
			}
		}
	}
}

// ponytail: collects the whole transitive closure before yielding, so memory
// grows with the number of groups one principal is in, not with group sizes.
// Stream it if a principal's groups ever number in the thousands.
func groupsOf(r kv.Reader, of entity.Principal) ([]entity.Principal, error) {
	seen := map[entity.PrincipalID]bool{of.ID: true}
	var out []entity.Principal
	queue := []entity.PrincipalID{of.ID}
	for len(queue) > 0 {
		member := queue[0]
		queue = queue[1:]
		prefix := memberOfPrefix(member)
		for kvp, err := range r.Scan(prefix, nil) {
			if err != nil {
				return nil, err
			}
			gid, _, err := keys.ReadID(kvp.Key[len(prefix):], keys.IDLen)
			if err != nil {
				return nil, err
			}
			id := entity.PrincipalID(gid)
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, entity.Principal{Kind: entity.PrincipalGroup, ID: id})
			queue = append(queue, id)
		}
	}
	return out, nil
}

// Grant is p's access to share. A grant naming p itself decides, even one
// of none; without one, the widest grant naming any of p's groups does.
// No grant at all is none.
func (p Principals) Grant(ctx context.Context, share entity.ShareID, of entity.Principal) (entity.ShareAccess, error) {
	var access entity.ShareAccess
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		groups, err := groupsOf(r, of)
		if err != nil {
			return err
		}
		access, err = grantFor(r, share, entity.Identity{Principal: of, Groups: groups})
		return err
	})
	return access, err
}

func grantIn(r kv.Reader, share entity.ShareID, p entity.PrincipalID) (entity.ShareGrant, bool, error) {
	b, err := r.Get(grantKey(share, p))
	if errors.Is(err, kv.ErrNotFound) {
		return entity.ShareGrant{}, false, nil
	}
	if err != nil {
		return entity.ShareGrant{}, false, err
	}
	g, err := decodeGrant(share, p, b)
	return g, err == nil, err
}
