package kvstore

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// ShareIndex is the filesystem service's read view of the share list: what
// admission and the pseudo-filesystem read on every call. It writes nothing,
// so the service reaches no control-plane write through it.
type ShareIndex struct{ s *Store }

func (s *Store) ShareIndex() ShareIndex { return ShareIndex{s} }

// Share is one share with its state and its export policy, read in one
// snapshot; a share with no stored policy has defaultPolicy's. A share being
// removed is still found, in its state; one never created is not found.
func (x ShareIndex) Share(ctx context.Context, id entity.ShareID) (entity.Share, entity.ExportPolicy, error) {
	var sh entity.Share
	p := defaultPolicy(id)
	err := x.s.kv.View(ctx, func(r kv.Reader) error {
		var err error
		if sh, err = shareIn(r, id); errors.Is(err, kv.ErrNotFound) {
			return merrs.NewNotFoundError(id.String(), "share")
		} else if err != nil {
			return err
		}
		b, ok, err := get(r, shareKey(id, keys.SXp))
		if err != nil || !ok {
			return err
		}
		p, err = decodePolicy(id, b)
		return err
	})
	return sh, p, err
}

// ByName is the share an SMB client names, compared under the share-name
// fold; ByPath the one an NFS client mounts. A share being removed has freed
// both, so neither finds it.
func (x ShareIndex) ByName(ctx context.Context, name string) (entity.ShareID, error) {
	return x.byIndex(ctx, nameKey(keys.NXShare, name), name)
}

func (x ShareIndex) ByPath(ctx context.Context, path string) (entity.ShareID, error) {
	return x.byIndex(ctx, pathKey(path), path)
}

func (x ShareIndex) byIndex(ctx context.Context, key []byte, what string) (entity.ShareID, error) {
	var id entity.ShareID
	err := x.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, key)
		if err != nil || !ok {
			if err == nil {
				err = merrs.NewNotFoundError(what, "share")
			}
			return err
		}
		id, err = decodeID(b)
		return err
	})
	return id, err
}

// Version is the share list's version alone: what a caller holding a list
// read at some version compares to know it is still current.
func (x ShareIndex) Version(ctx context.Context) (entity.ShareList, error) {
	v, err := counter(ctx, x.s.kv, shareListKey)
	return entity.ShareList{Version: v}, err
}

// List is every share the name index holds, with the share list's version,
// read in one snapshot, so the version is the one the list was read at.
//
// ponytail: reads every share in one transaction, O(shares). Page it, as the
// control plane's listing does, if an installation's share count makes the
// transaction long.
func (x ShareIndex) List(ctx context.Context) (entity.ShareList, []entity.Share, error) {
	var (
		v   uint64
		out []entity.Share
	)
	err := x.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, shareListKey)
		if err != nil {
			return err
		}
		if ok {
			if v, err = decodeCounter(b); err != nil {
				return err
			}
		}
		for row, err := range r.Scan([]byte{keys.KindNX, keys.NXShare}, nil) {
			if err != nil {
				return err
			}
			id, err := decodeID(row.Value)
			if err != nil {
				return err
			}
			sh, err := shareIn(r, id)
			if err != nil {
				return err
			}
			out = append(out, sh)
		}
		return nil
	})
	return entity.ShareList{Version: v}, out, err
}

// Netgroup is the netgroup of that name; found is false for none, which a
// client rule naming it treats as failing closed.
func (x ShareIndex) Netgroup(ctx context.Context, name string) (g entity.Netgroup, found bool, err error) {
	err = x.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, netgroupKey(name))
		if err != nil || !ok {
			return err
		}
		g, err = decodeNetgroup(name, b)
		found = err == nil
		return err
	})
	return g, found, err
}

// Installation is the installation's identity record.
func (s *Store) Installation(ctx context.Context) (entity.Installation, error) {
	var in entity.Installation
	err := s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(installationKey)
		if err != nil {
			return err
		}
		in, err = decodeInstallation(b)
		return err
	})
	return in, err
}
