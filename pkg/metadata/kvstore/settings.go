package kvstore

import (
	"context"
	"iter"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Netgroups lists netgroups in name order, resuming after the name after.
func (c ControlPlane) Netgroups(ctx context.Context, after string) iter.Seq2[entity.Netgroup, error] {
	prefix := []byte{keys.KindNG}
	var cursor []byte
	if after != "" {
		cursor = netgroupKey(after)
	}
	return pages(ctx, c.s.kv, prefix, cursor, func(_ kv.Reader, row kv.KeyValue) (entity.Netgroup, error) {
		name, _, err := keys.ReadString(row.Key[len(prefix):])
		if err != nil {
			return entity.Netgroup{}, err
		}
		return decodeNetgroup(string(name), row.Value)
	})
}

// PutNetgroup creates or replaces a netgroup, raising its version.
func (c ControlPlane) PutNetgroup(ctx context.Context, g entity.Netgroup) error {
	if g.Name == "" {
		return merrs.NewInvalidArgumentError("a netgroup needs a name")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		key := netgroupKey(g.Name)
		b, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		var old entity.Netgroup
		if ok {
			if old, err = decodeNetgroup(g.Name, b); err != nil {
				return err
			}
		}
		g.Version = nextVersion(tx, old.Version)
		return tx.Set(key, encodeNetgroup(g))
	})
}

// DeleteNetgroup removes a netgroup. No rule naming it is rewritten: such a
// rule then fails closed, an allow matching no host and a deny every host.
func (c ControlPlane) DeleteNetgroup(ctx context.Context, name string) error {
	return c.s.kv.Update(ctx, func(tx kv.Txn) error { return tx.Delete(netgroupKey(name)) })
}

// Settings lists the settings at one scope, in key order.
func (c ControlPlane) Settings(ctx context.Context, scope entity.SettingScope) iter.Seq2[entity.Setting, error] {
	prefix := scopePrefix(scope)
	return pages(ctx, c.s.kv, prefix, nil, func(_ kv.Reader, row kv.KeyValue) (entity.Setting, error) {
		key, _, err := keys.ReadString(row.Key[len(prefix):])
		if err != nil {
			return entity.Setting{}, err
		}
		return decodeSetting(scope, string(key), row.Value)
	})
}

// PutSetting writes one setting, raising its generation and the settings
// generation every node polls, in one transaction.
func (c ControlPlane) PutSetting(ctx context.Context, s entity.Setting) error {
	if s.Scope.Kind.String() == "" || (s.Scope.Kind == entity.ScopeInstallation) != (s.Scope.ID == [16]byte{}) || s.Key == "" {
		return merrs.NewInvalidArgumentError("a setting needs a key and a scope, with an ID below installation scope")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		key := settingKey(s.Scope, s.Key)
		b, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		var old entity.Setting
		if ok {
			if old, err = decodeSetting(s.Scope, s.Key, b); err != nil {
				return err
			}
		}
		s.Generation = old.Generation + 1
		if err := tx.Set(key, encodeSetting(s)); err != nil {
			return err
		}
		return raise(tx, cfgGenKey)
	})
}

// SettingsGeneration is raised by every setting change; a node that sees it
// move re-reads the settings.
func (c ControlPlane) SettingsGeneration(ctx context.Context) (uint64, error) {
	return counter(ctx, c.s.kv, cfgGenKey)
}
