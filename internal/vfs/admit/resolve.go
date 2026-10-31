package admit

import (
	"context"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// Principals is the reads Resolve makes, each answering what it read or that
// there is none, and any other failure as the error the call answers.
type Principals interface {
	// ByID is the principal holding a uid or gid.
	ByID(ctx context.Context, scheme entity.ProtocolScheme, n uint32) (p entity.Principal, found bool, err error)
	// User is p's user record; not found for a deleted one.
	User(ctx context.Context, p entity.Principal) (u entity.User, found bool, err error)
	// Grant is p's access to share as a signed-in principal: its own
	// grant, else its groups', else authenticated users', else everyone's.
	Grant(ctx context.Context, share entity.ShareID, p entity.Principal) (entity.ShareAccess, error)
	// Groups is every group p is in.
	Groups(ctx context.Context, p entity.Principal) ([]entity.Principal, error)
}

// Resolve is admission's resolve step: the caller id acts as on share,
// under policy p, narrowed to read-only by ro, and the principal it mapped
// to before any squash. A proven caller maps to the principal its sign-in
// bound, a claimed one to the user holding its claimed uid, and anyone else
// to none; a disabled or deleted user is refused. A caller is root when it
// claims uid 0 or proves a user holding uid 0, and a root squash squashes
// either. A caller the policy squashes, one mapped to no principal, or one
// proving a user that holds no uid, such as a machine principal, acts as the
// policy's anonymous principal, with its groups and no flag; a policy naming
// none refuses it, as it does a deleted or disabled one.
// Otherwise a proven caller acts as itself, authenticated, admin where its
// share grant names admin, and privileged where it is admin or root; a
// claimed one acts as its user and the group holding its gid, privileged
// when its uid is 0. A proven caller's share grant is read for the principal
// it mapped to before any squash, a claimed caller's for whom it acts as.
//
// ponytail: resolves from the store's export policy, protocol IDs, groups
// and grant alone, uncached, on every call; RFC 18's resolver replaces it,
// adding the cache and directories.
//
// decision: the resolver is this function of admission, over the reads it
// declares, not a component the filesystem is handed: there is no
// Deps.Identity, as one resolver behind an interface is a seam with one
// implementation. Overturned by RFC 18's resolver landing, which replaces it.
//
// decision: a squashed proven caller whose own grant is none is refused
// here, at admission, and so is not audited, while an unsquashed one is
// refused by the store's check and audited there. The store reads the grants
// of the principal a caller acts as, and a squashed caller acts as another;
// it carries its own grant in Access, which cannot say none. Overturned once
// admission's refusals are audited, or the store takes a grant of none.
//
// decision: a claimed caller's supplementary gids are not used: its groups
// are its principal's memberships in the store, and only its primary gid
// names the credential's group. A claim proves nothing, and a membership the
// store does not hold is one the operator never granted. Trust them if an
// installation without principals' memberships needs AUTH_SYS groups.
//
// decision: a claimed uid 0 no user holds is not anonymous, as an unmapped
// uid otherwise is: unsquashed, it is privileged with no principal, so the
// files it creates are owned by the zero principal and its share grant is
// the one its gid's group holds. That keeps what no_root_squash asks of an
// installation that never named a root user. Overturned once the store
// holds a well-known root principal for uid 0 to map to.
func Resolve(ctx context.Context, pr Principals, id vfs.Identity, share entity.ShareID, p entity.ExportPolicy, ro entity.ReadOnlyCause) (c entity.Identity, mapped entity.Principal, err error) {
	proven := id.Proof >= vfs.ProofSecret
	root := false
	var group entity.Principal
	switch {
	case proven:
		mapped = id.Evidence.Principal
	case id.Proof == vfs.ProofClaimed:
		root = id.Claims.UID == 0
		if mapped, _, err = pr.ByID(ctx, entity.SchemeUID, id.Claims.UID); err != nil {
			return c, mapped, err
		}
		if group, _, err = pr.ByID(ctx, entity.SchemeGID, id.Claims.GID); err != nil {
			return c, mapped, err
		}
	}
	u, err := live(ctx, pr, mapped)
	if err != nil {
		return c, mapped, err
	}
	root = root || proven && slices.Contains(u.IDs, entity.ProtocolID{Scheme: entity.SchemeUID, Value: "0"})
	fileUser := proven && slices.ContainsFunc(u.IDs, func(i entity.ProtocolID) bool { return i.Scheme == entity.SchemeUID })
	squashed := p.Squash == entity.SquashAll || root && p.Squash == entity.SquashRoot
	var access entity.ShareAccess
	if fileUser {
		if access, err = pr.Grant(ctx, share, mapped); err != nil {
			return c, mapped, err
		}
		if squashed && access == entity.AccessNone {
			return c, mapped, vfs.ErrAccess
		}
	}
	switch {
	case squashed || proven && !fileUser || mapped.IsZero() && !root:
		if p.Anonymous.IsZero() {
			return c, mapped, vfs.ErrAccess
		}
		c = entity.Identity{Principal: p.Anonymous}
		if squashed && proven {
			c.Access = access
		}
		if _, err := live(ctx, pr, c.Principal); err != nil {
			return c, mapped, err
		}
	case proven:
		c.Principal, c.Authenticated = mapped, true
		c.Admin = access == entity.AccessAdmin
		c.Privileged = c.Admin || root
	default:
		c.Principal, c.Group, c.Privileged = mapped, group, root
	}
	c.ReadOnly = ro
	if c.Principal.IsZero() {
		return c, mapped, nil
	}
	c.Groups, err = pr.Groups(ctx, c.Principal)
	return c, mapped, err
}

// live is p's user record, refusing a user disabled or deleted; any other
// principal passes, with the zero record.
func live(ctx context.Context, pr Principals, p entity.Principal) (entity.User, error) {
	if p.Kind != entity.PrincipalUser {
		return entity.User{}, nil
	}
	u, found, err := pr.User(ctx, p)
	if err != nil {
		return u, err
	}
	if !found || u.Disabled {
		return u, vfs.ErrAccess
	}
	return u, nil
}
