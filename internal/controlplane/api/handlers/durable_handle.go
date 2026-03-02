package handlers

import (
	"encoding/hex"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// durableHandleStoreProvider is a local interface for metadata stores that
// expose a DurableHandleStore accessor via type assertion.
type durableHandleStoreProvider interface {
	DurableHandleStore() lock.DurableHandleStore
}

// DurableHandleHandler provides REST API endpoints for listing and
// force-closing SMB3 durable handles across all metadata stores.
type DurableHandleHandler struct {
	rt *runtime.Runtime
}

// NewDurableHandleHandler creates a new durable handle handler.
func NewDurableHandleHandler(rt *runtime.Runtime) *DurableHandleHandler {
	return &DurableHandleHandler{rt: rt}
}

// durableHandleSummary is the JSON representation of a durable handle.
type durableHandleSummary struct {
	ID             string `json:"id"`
	FileID         string `json:"file_id"`
	Path           string `json:"path"`
	ShareName      string `json:"share_name"`
	Username       string `json:"username"`
	IsV2           bool   `json:"is_v2"`
	CreatedAt      string `json:"created_at"`
	DisconnectedAt string `json:"disconnected_at"`
	TimeoutMs      uint32 `json:"timeout_ms"`
	RemainingMs    int64  `json:"remaining_ms"`
}

// toSummary converts a PersistedDurableHandle to its API representation.
func toSummary(h *lock.PersistedDurableHandle) durableHandleSummary {
	now := time.Now()
	expiresAt := h.DisconnectedAt.Add(time.Duration(h.TimeoutMs) * time.Millisecond)
	remaining := expiresAt.Sub(now).Milliseconds()
	if remaining < 0 {
		remaining = 0
	}

	return durableHandleSummary{
		ID:             h.ID,
		FileID:         hex.EncodeToString(h.FileID[:]),
		Path:           h.Path,
		ShareName:      h.ShareName,
		Username:       h.Username,
		IsV2:           h.IsV2,
		CreatedAt:      h.CreatedAt.UTC().Format(time.RFC3339),
		DisconnectedAt: h.DisconnectedAt.UTC().Format(time.RFC3339),
		TimeoutMs:      h.TimeoutMs,
		RemainingMs:    remaining,
	}
}

// List handles GET /api/v1/durable-handles.
//
// Returns all active durable handles aggregated across all metadata stores.
// Supports optional ?share=<name> query parameter to filter by share.
func (dh *DurableHandleHandler) List(w http.ResponseWriter, r *http.Request) {
	shareFilter := r.URL.Query().Get("share")

	var summaries []durableHandleSummary

	for _, storeName := range dh.rt.ListMetadataStores() {
		metaStore, err := dh.rt.GetMetadataStore(storeName)
		if err != nil {
			continue
		}

		provider, ok := metaStore.(durableHandleStoreProvider)
		if !ok {
			continue
		}

		ds := provider.DurableHandleStore()

		var handles []*lock.PersistedDurableHandle
		if shareFilter != "" {
			handles, err = ds.ListDurableHandlesByShare(r.Context(), shareFilter)
		} else {
			handles, err = ds.ListDurableHandles(r.Context())
		}
		if err != nil {
			logger.Warn("DurableHandleHandler.List: failed to list handles",
				"store", storeName, "error", err)
			continue
		}

		for _, h := range handles {
			summaries = append(summaries, toSummary(h))
		}
	}

	if summaries == nil {
		summaries = []durableHandleSummary{}
	}

	WriteJSONOK(w, summaries)
}

// ForceClose handles DELETE /api/v1/durable-handles/{id}.
//
// Finds the durable handle by ID across all metadata stores, performs cleanup,
// and deletes it. Returns 204 on success, 404 if not found.
func (dh *DurableHandleHandler) ForceClose(w http.ResponseWriter, r *http.Request) {
	handleID := chi.URLParam(r, "id")
	if handleID == "" {
		BadRequest(w, "Handle ID is required")
		return
	}

	// Search for the handle across all metadata stores
	for _, storeName := range dh.rt.ListMetadataStores() {
		metaStore, err := dh.rt.GetMetadataStore(storeName)
		if err != nil {
			continue
		}

		provider, ok := metaStore.(durableHandleStoreProvider)
		if !ok {
			continue
		}

		ds := provider.DurableHandleStore()
		h, err := ds.GetDurableHandle(r.Context(), handleID)
		if err != nil {
			logger.Warn("DurableHandleHandler.ForceClose: error looking up handle",
				"store", storeName, "id", handleID, "error", err)
			continue
		}

		if h == nil {
			continue
		}

		// Found the handle -- perform cleanup and delete

		// Release byte-range locks
		metaSvc := dh.rt.GetMetadataService()
		if len(h.MetadataHandle) > 0 && metaSvc != nil {
			_ = metaSvc.UnlockAllForSession(r.Context(), h.MetadataHandle, 0)
		}

		// Delete from store
		if err := ds.DeleteDurableHandle(r.Context(), handleID); err != nil {
			InternalServerError(w, "Failed to delete durable handle")
			return
		}

		logger.Info("DurableHandleHandler.ForceClose: handle force-closed",
			"id", handleID, "path", h.Path, "share", h.ShareName)

		WriteNoContent(w)
		return
	}

	NotFound(w, "Durable handle not found")
}
