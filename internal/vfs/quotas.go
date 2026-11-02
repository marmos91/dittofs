package vfs

import (
	"context"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/quota"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// capacity is the store's Capacity as quota reads it.
type capacity struct{ kvstore.Capacity }

func (c capacity) Quota(ctx context.Context, share entity.ShareID, t quota.Target) (entity.Quota, time.Time, error) {
	return c.Capacity.Quota(ctx, share, kvstore.QuotaTarget(t))
}

func (c capacity) Usage(ctx context.Context, share entity.ShareID, t quota.Target) (entity.Usage, error) {
	switch {
	case !t.Principal.IsZero():
		return c.PrincipalUsage(ctx, share, t.Principal)
	case t.Project != 0:
		return c.ProjectUsage(ctx, share, t.Project)
	}
	return c.Capacity.Usage(ctx, share)
}

func (c capacity) SoftExceeded(ctx context.Context, share entity.ShareID, t quota.Target, above bool) (time.Time, error) {
	return c.Capacity.SoftExceeded(ctx, share, kvstore.QuotaTarget(t), above)
}

// reserve reserves add charged to c for the caller a admitted, the store's
// refusals answered as the service's.
func (fs *Filesystem) reserve(ctx context.Context, a *admitted, c quota.Charge, add entity.Usage) (*quota.Reservation, error) {
	r, err := fs.quota.Reserve(ctx, a.caller.Proved, c, add)
	return r, storeErr(err)
}

// charged is whom a change of f's content charges.
func charged(f vfs.File) quota.Charge {
	return quota.Charge{Share: f.Share, Owner: f.Owner, Group: f.Group, Project: f.Project}
}

// newOwner is whom a create of ca in d by a charges: the owner and group the
// new file will have, never d's own. The owner is the caller, or the one
// the create names; the group the one it names, else d's when d has setgid,
// else the credential's, else the caller's primary group, else the caller
// itself, a group of one.
//
// ponytail: the store decides the new file's group again, by the same rule,
// in the create's transaction, so a change to one rule and not the other
// charges a group the file does not have. Have the create take the owner
// and group decided here once a second caller needs them.
func (fs *Filesystem) newOwner(ctx context.Context, a *admitted, d vfs.File, ca vfs.CreateArgs) (quota.Charge, error) {
	id := a.caller.Identity
	c := quota.Charge{Share: d.Share, Owner: id.Principal, Group: id.Group, Project: d.Project}
	if ca.Attrs.Changes(entity.AttrOwner) {
		c.Owner = ca.Attrs.Owner
	}
	switch {
	case ca.Attrs.Changes(entity.AttrGroup):
		c.Group = ca.Attrs.Group
	case d.Mode&entity.ModeSetgid != 0 && !d.Group.IsZero():
		c.Group = d.Group
	case c.Group.IsZero() && id.Principal.Kind == entity.PrincipalUser:
		u, err := fs.meta.Principals().User(ctx, id.Principal)
		if err != nil && !merrs.IsNotFoundError(err) {
			return c, storeErr(err)
		}
		c.Group = u.Primary
	}
	if c.Group.IsZero() {
		c.Group = id.Principal
	}
	return c, nil
}
