package kvstore

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
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
