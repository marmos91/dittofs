// Package journalwrap makes the journal satisfy the engine's Journal, for the
// composition root and the engine's test fixture alike.
//
// The journal's Share and Reserve return its concrete handle and reservation,
// and Go does not accept a concrete result for a method whose interface
// declares an interface result, so those two calls are wrapped. Every other
// method is the journal's own.
package journalwrap

import (
	"github.com/marmos91/dittofs/pkg/block/enginev2"
	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// Wrap returns j as the engine's Journal. The handles its Share returns are
// the engine's Handle, and their Reserve returns the engine's Reservation.
func Wrap(j *journalv2.Journal) enginev2.Journal { return journal{j} }

var (
	_ enginev2.Handle      = handle{}
	_ enginev2.Reservation = (*journalv2.Reservation)(nil)
)

type journal struct{ *journalv2.Journal }

func (j journal) Share(tag journalv2.ShareTag, limit int64, floor journalv2.Version, policy journalv2.SyncPolicy) (enginev2.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor, policy)
	if err != nil {
		return nil, err
	}
	return handle{h}, nil
}

type handle struct{ *journalv2.Handle }

func (h handle) Reserve(n int64, writes int) (enginev2.Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}
	return r, nil
}
