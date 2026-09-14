package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/pending"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// createDRC and cachedCreate name this package's instantiation of the
// replay cache. The cache is generic over what it stores so that the pending
// package need not know the SMB wire types, which is what lets it stand as its
// own package at all.
type createDRC = pending.CreateDRC[*CreateResponse, *OpenFile]

type cachedCreate = pending.CachedCreateResponse[*CreateResponse, *OpenFile]

// storeCreateReplay caches a CREATE response for replay, but only a successful
// one: a failed CREATE must run the normal handler path on replay, where it may
// legitimately succeed the second time (MS-SMB2 §3.3.5.9). Caching a failure
// would pin the error for the life of the replay window.
//
// The rule lives here rather than in the cache because deciding it requires
// reading a status off the concrete response type, which the cache deliberately
// does not know. Both CREATE completion paths go through this so neither can
// drift from the rule.
func (h *Handler) storeCreateReplay(sessionID uint64, createGuid [16]byte, resp *CreateResponse, openFile *OpenFile) {
	if h.CreateDRC == nil || resp == nil || resp.Status != types.StatusSuccess {
		return
	}
	h.CreateDRC.Record(sessionID, createGuid, resp, openFile)
}
