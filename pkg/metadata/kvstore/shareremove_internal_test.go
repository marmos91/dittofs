package kvstore

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

func TestDeleteShareGates(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		rows  [][]byte
		disp  entity.SnapshotDisposition
		allow bool
	}{
		{"snapshots, refuse", [][]byte{{keys.SSnap, 1}}, entity.SnapshotsRefuse, false},
		{"snapshots, delete", [][]byte{{keys.SSnap, 1}}, entity.SnapshotsDelete, true},
		{"snapshots, detach", [][]byte{{keys.SSnap, 1}}, entity.SnapshotsDetach, true},
		{"use record, delete", [][]byte{{keys.SSnap, 1}, {keys.SUse, 1}}, entity.SnapshotsDelete, false},
		{"use record, detach", [][]byte{{keys.SSnap, 1}, {keys.SUse, 1}}, entity.SnapshotsDetach, true},
		{"re-home running", [][]byte{{keys.SRh}}, entity.SnapshotsDelete, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openStore(t)
			share := newShare(t, s)
			var kvs [][]byte
			for _, sub := range c.rows {
				kvs = append(kvs, append(shareKey(share, sub[0]), sub[1:]...), []byte{1})
			}
			rows(t, s, kvs...)
			err := s.ControlPlane().DeleteShare(ctx, share, c.disp)
			if (err == nil) != c.allow {
				t.Fatalf("DeleteShare = %v, want allowed %v", err, c.allow)
			}
		})
	}
}

// A move pausing one of the share's namespaces refuses the removal.
func TestDeleteSharePausedNamespace(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := newShare(t, s)
	var nss shareNamespaces
	if err := s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(namespacesKey(share))
		if err != nil {
			return err
		}
		nss, err = decodeNamespaces(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(nss.Namespaces) != 1 || nss.Write != 1 || nss.Namespaces[0].Namespace.IsZero() {
		t.Fatalf("a new share's namespaces: %+v", nss)
	}
	rows(t, s, gcPauseKey(nss.Namespaces[0].Namespace), []byte{1})
	if err := s.ControlPlane().DeleteShare(ctx, share, entity.SnapshotsDelete); !errors.Is(err, ErrMoving) {
		t.Fatalf("DeleteShare under a pause: %v", err)
	}
}
