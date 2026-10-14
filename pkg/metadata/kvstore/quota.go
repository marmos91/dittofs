package kvstore

import (
	"context"
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// QuotaTarget is what a quota limits in a share: a user or group, or a
// project. Exactly one is set.
type QuotaTarget struct {
	Principal entity.Principal
	Project   uint64
}

// The principal-or-project tag that starts a quota key's suffix.
const (
	quotaPrincipal byte = 0x01
	quotaProject   byte = 0x02
)

// quotaSuffix is t's tagged key component: the tag, then a PrincipalID or
// the project number.
//
// ponytail: a share-wide quota has no tag in the key format, so only user,
// group and project quotas are kept. Add one when the format gives the
// share's own limit a key.
func quotaSuffix(t QuotaTarget) ([]byte, error) {
	switch {
	case t.Project == 0 && (t.Principal.Kind == entity.PrincipalUser || t.Principal.Kind == entity.PrincipalGroup):
		return keys.ID([]byte{quotaPrincipal}, t.Principal.ID[:]), nil
	case t.Project != 0 && t.Principal == (entity.Principal{}):
		return keys.Uint64([]byte{quotaProject}, t.Project), nil
	}
	return nil, merrs.NewInvalidArgumentError("a quota limits a user or group, or a project")
}

func quotaKey(share entity.ShareID, sub byte, suffix []byte) []byte {
	return append(shareKey(share, sub), suffix...)
}

// A quota's value: the principal's kind, which its key's ID lacks, then the
// limits. A project quota's kind is zero.
func encodeQuota(q entity.Quota) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(q.Principal.Kind))
	for _, l := range []entity.Limit{q.Hard, q.Soft, q.Advisory} {
		w.Int64(l.Bytes).Int64(l.Files)
	}
	return w.Int64(int64(q.Grace)).Encode()
}

func decodeQuota(share entity.ShareID, suffix, b []byte) (entity.Quota, error) {
	r, err := open(b)
	if err != nil {
		return entity.Quota{}, err
	}
	q := entity.Quota{Share: share}
	kind := entity.PrincipalKind(r.Uint8())
	limits := []*entity.Limit{&q.Hard, &q.Soft, &q.Advisory}
	for _, l := range limits {
		l.Bytes, l.Files = r.Int64(), r.Int64()
	}
	q.Grace = time.Duration(r.Int64())
	if len(suffix) == 0 {
		return q, errMalformed
	}
	switch suffix[0] {
	case quotaPrincipal:
		id, _, err := keys.ReadID(suffix[1:], keys.IDLen)
		if err != nil {
			return q, err
		}
		q.Principal = entity.Principal{Kind: kind, ID: entity.PrincipalID(id)}
	case quotaProject:
		if q.Project, _, err = keys.ReadUint64(suffix[1:]); err != nil {
			return q, err
		}
	default:
		return q, errMalformed
	}
	return q, r.Done()
}

// SetQuota writes a share's quota on a user, a group or a project, and a
// quota that limits nothing deletes it. The share must be live, and a user
// or group must exist: its record is read, so a deletion of it conflicts.
//
// decision: a changed soft limit restarts its grace, since the time usage
// went above the old one says nothing about the new one; any other change
// keeps it, so editing a hard limit never extends a grace already running.
func (c ControlPlane) SetQuota(ctx context.Context, q entity.Quota) error {
	suffix, err := quotaSuffix(QuotaTarget{q.Principal, q.Project})
	if err != nil {
		return err
	}
	for _, l := range []entity.Limit{q.Hard, q.Soft, q.Advisory} {
		if l.Bytes < 0 || l.Files < 0 {
			return merrs.NewInvalidArgumentError("a quota limit is not negative")
		}
	}
	if q.Grace < 0 {
		return merrs.NewInvalidArgumentError("a quota's grace is not negative")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, ok, err := get(tx, shareKey(q.Share, keys.SInfo)); err != nil || !ok {
			if err == nil {
				err = merrs.NewNotFoundError(q.Share.String(), "share")
			}
			return err
		}
		if err := liveShare(tx, q.Share); err != nil {
			return err
		}
		if q.Project == 0 {
			if _, ok, err := get(tx, principalKey(q.Principal)); err != nil || !ok {
				return notFound(err, q.Principal)
			}
		}
		key, since := quotaKey(q.Share, keys.SQt, suffix), quotaKey(q.Share, keys.SQx, suffix)
		if q.Unlimited() {
			if err := tx.Delete(key); err != nil {
				return err
			}
			return tx.Delete(since)
		}
		b, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		if ok {
			old, err := decodeQuota(q.Share, suffix, b)
			if err != nil {
				return err
			}
			if old.Soft != q.Soft {
				if err := tx.Delete(since); err != nil {
					return err
				}
			}
		}
		return tx.Set(key, encodeQuota(q))
	})
}

// Quotas lists a share's quotas, the users' and groups' before the
// projects'.
func (c ControlPlane) Quotas(ctx context.Context, share entity.ShareID) iter.Seq2[entity.Quota, error] {
	pre := shareKey(share, keys.SQt)
	return pages(ctx, c.s.kv, pre, nil, func(_ kv.Reader, row kv.KeyValue) (entity.Quota, error) {
		return decodeQuota(share, row.Key[len(pre):], row.Value)
	})
}

// Quota is the quota on t in a share, the zero Quota when there is none, and
// since, when usage first went above its soft limit, zero when it isn't
// above it.
func (c Capacity) Quota(ctx context.Context, share entity.ShareID, t QuotaTarget) (q entity.Quota, since time.Time, err error) {
	suffix, err := quotaSuffix(t)
	if err != nil {
		return q, since, err
	}
	q = entity.Quota{Share: share, Principal: t.Principal, Project: t.Project}
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, quotaKey(share, keys.SQt, suffix))
		if err != nil || !ok {
			return err
		}
		if q, err = decodeQuota(share, suffix, b); err != nil {
			return err
		}
		b, ok, err = get(r, quotaKey(share, keys.SQx, suffix))
		if err != nil || !ok {
			return err
		}
		since, err = decodeSince(b)
		return err
	})
	return q, since, err
}

// SoftExceeded records whether usage of t is above its soft limit, as the
// filesystem service finds it: above, it returns when usage first went
// above, the store's time now when nothing was recorded yet; not above, it
// clears the record and returns zero. Each call writes only when the answer
// changes.
func (c Capacity) SoftExceeded(ctx context.Context, share entity.ShareID, t QuotaTarget, above bool) (since time.Time, err error) {
	suffix, err := quotaSuffix(t)
	if err != nil {
		return since, err
	}
	key := quotaKey(share, keys.SQx, suffix)
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		since = time.Time{}
		b, ok, err := get(tx, key)
		switch {
		case err != nil:
			return err
		case ok && above:
			since, err = decodeSince(b)
			return err
		case ok:
			return tx.Delete(key)
		case above:
			since = tx.Now()
			return tx.Set(key, encodeSince(since))
		}
		return nil
	})
	return since, err
}

func encodeSince(t time.Time) []byte { return putTime(codec.NewWriter(v1), t).Encode() }

func decodeSince(b []byte) (time.Time, error) {
	r, err := open(b)
	if err != nil {
		return time.Time{}, err
	}
	t := readTime(r)
	return t, r.Done()
}

// dropQuotas deletes p's quota records in every share, as deleting p does.
// Each is read first, tracked, so a quota set for p meanwhile conflicts.
//
// ponytail: it reads two keys in every share, O(shares), since no index
// names the shares holding a principal's quotas. Add a reverse index, as
// grants have, when installations hold enough shares for a principal
// deletion to show it.
func dropQuotas(tx kv.Txn, p entity.Principal) error {
	suffix, err := quotaSuffix(QuotaTarget{Principal: p})
	if err != nil {
		return err
	}
	var shares []entity.ShareID
	for row, err := range tx.Scan([]byte{keys.KindNX, keys.NXShare}, nil) {
		if err != nil {
			return err
		}
		id, err := decodeID(row.Value)
		if err != nil {
			return err
		}
		shares = append(shares, entity.ShareID(id))
	}
	for _, share := range shares {
		for _, sub := range []byte{keys.SQt, keys.SQx} {
			key := quotaKey(share, sub, suffix)
			if _, ok, err := get(tx, key); err != nil {
				return err
			} else if ok {
				if err := tx.Delete(key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
