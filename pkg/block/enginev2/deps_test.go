package enginev2

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
)

// The components the root will pass satisfy the interfaces Deps declares, so
// a signature that drifts from its component fails to build here rather than
// at the first composition. The journal's Share and Reserve return its
// concrete types, which Go does not accept for an interface result, so those
// two are wrapped; every other method is the journal's own.
var (
	_ Journal     = realJournal{}
	_ Handle      = realHandle{}
	_ Reservation = (*journalv2.Reservation)(nil)
	_ Existence   = kvstore.Existence{}
	_ Content     = kvstore.Content{}
	_ Opens       = openstate.Opens(nil)
	_ Clock       = journalv2.Clock(nil)
)

type realJournal struct{ *journalv2.Journal }

func (j realJournal) Share(tag journalv2.ShareTag, limit int64, floor journalv2.Version, policy journalv2.SyncPolicy) (Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor, policy)
	return realHandle{h}, err
}

type realHandle struct{ *journalv2.Handle }

func (h realHandle) Reserve(n int64, writes int) (Reservation, error) {
	return h.Handle.Reserve(n, writes)
}
