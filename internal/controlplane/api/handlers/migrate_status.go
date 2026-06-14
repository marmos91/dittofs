package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/migrate"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// isValidShareName rejects names containing path separators or traversal
// segments. Defense-in-depth: the share registry lookup is keyed by
// exact match, but we want to fail fast on obviously hostile inputs
// before they reach store-layer code that joins the name into a path.
func isValidShareName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// MigrateStatusRuntime is the narrow Runtime surface MigrateStatusHandler
// consumes. Defining it here (rather than depending on *runtime.Runtime
// directly) keeps the handler unit-testable: tests substitute a fake
// that records calls and returns canned responses, mirroring the
// BlockGCRuntime pattern in block_gc.go.
//
// BLOCKER 2 fix from review: the methods reference here
// match the Runtime's actual exported surface. `MetadataStoreFor` and
// `LocalStoreDirFor` from the pre-revision plan never existed —
// `GetMetadataStoreForShare` is the canonical method, and `LocalStoreDir`
// is added in this plan via Runtime.LocalStoreDir delegating to the
// shares service.
type MigrateStatusRuntime interface {
	// GetMetadataStoreForShare returns the metadata store backing the
	// named share. Used to read the share's BlockLayout and
	// to walk the file tree for the FilesTotal field.
	GetMetadataStoreForShare(shareName string) (metadata.Store, error)

	// LocalStoreDir returns the per-share on-disk data directory hosting
	// the migration journal (new accessor). Empty string +
	// nil error indicates a memory-backed share — handler short-circuits
	// the journal read.
	LocalStoreDir(shareName string) (string, error)
}

// MigrateStatusHandler handles the per-share migration status endpoint:
// GET /api/v1/blockstore/migrate/status?share=NAME.
//
// Response shape mirrors apiclient.MigrateStatusResponse so dittofs-pro
// and dfsctl share a single contract. Admin-only auth is enforced by
// the router placing the route inside JWTAuth + RequireAdmin middleware.
//
// This file imports `pkg/block/migrate` (the journal type, placed
// in pkg/ from day one for exactly this reason) rather than
// `cmd/dfsctl/...`, which Go's build system forbids from being imported
// by internal/.
type MigrateStatusHandler struct {
	rt MigrateStatusRuntime
}

// NewMigrateStatusHandler constructs a handler bound to the given
// Runtime surface. Pass a nil-safe value: the handler refuses requests
// when rt is nil so the server can still boot in degraded modes.
func NewMigrateStatusHandler(rt MigrateStatusRuntime) *MigrateStatusHandler {
	return &MigrateStatusHandler{rt: rt}
}

// migrateStatusResponse is the JSON response body. Field names + tags
// must stay aligned with apiclient.MigrateStatusResponse — both
// surfaces share a single contract.
type migrateStatusResponse struct {
	Share           string `json:"share"`
	BlockLayout     string `json:"block_layout"`
	FilesTotal      int    `json:"files_total"`
	FilesDone       int    `json:"files_done"`
	FilesSkipped    int    `json:"files_skipped"`
	BytesUploaded   uint64 `json:"bytes_uploaded"`
	BytesDeduped    uint64 `json:"bytes_deduped"`
	JournalPresent  bool   `json:"journal_present"`
	SnapshotPresent bool   `json:"snapshot_present"`
	LastCommitAt    string `json:"last_commit_at,omitempty"`
}

// fileWalkTimeout is the bound on the FilesTotal walk per request.
// On timeout, FilesTotal is set to -1 (the documented incomplete-walk
// sentinel) and the response still ships with everything else valid.
// Operators can bypass the walk entirely via ?with_total=false.
const fileWalkTimeout = 30 * time.Second

// Status handles GET /api/v1/blockstore/migrate/status.
//
// Query parameters:
//   - share (required): the share name to query.
//   - with_total (optional, default true): when "false", skip the file
//     walk and leave FilesTotal at zero. Useful on TB-scale shares
//     where the operator only wants the journal aggregate.
//
// Status codes:
//   - 200 OK with migrateStatusResponse on success (including the
//     no-journal steady state — that's not an error)
//   - 400 Bad Request when ?share= is missing
//   - 404 Not Found when the share is unknown to the metadata store
//   - 500 Internal Server Error on metadata-read failure
func (h *MigrateStatusHandler) Status(w http.ResponseWriter, r *http.Request) {
	if h.rt == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	share := r.URL.Query().Get("share")
	if share == "" {
		BadRequest(w, "share is required")
		return
	}
	if !isValidShareName(share) {
		BadRequest(w, "invalid share name")
		return
	}

	// 1. Read BlockLayout via the existing GetMetadataStoreForShare
	//    method (BLOCKER 2 fix — `MetadataStoreFor` from the
	//    pre-revision plan does not exist on the Runtime).
	mds, err := h.rt.GetMetadataStoreForShare(share)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+share)
			return
		}
		InternalServerError(w, "metadata store lookup: "+err.Error())
		return
	}

	opts, err := mds.GetShareOptions(r.Context(), share)
	if err != nil {
		InternalServerError(w, "read share options: "+err.Error())
		return
	}

	resp := migrateStatusResponse{
		Share: share,
	}
	if opts != nil {
		// ParseBlockLayout-coerced enum stringifies to the on-the-wire
		// value ("legacy" | "cas-only").
		resp.BlockLayout = string(opts.BlockLayout)
	}
	if resp.BlockLayout == "" {
		// Pre-Phase-14 share row → legacy (matches every metadata
		// backend's coerce-on-read semantics).
		resp.BlockLayout = string(metadata.BlockLayoutLegacy)
	}

	// 2. Resolve the journal directory via the new LocalStoreDir
	//    accessor (BLOCKER 2 fix — `LocalStoreDirFor` did not exist).
	//    An empty path is the documented "no on-disk journal"
	//    response for memory-backed shares; not an error.
	journalDir, err := h.rt.LocalStoreDir(share)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+share)
			return
		}
		// Any other lookup failure indicates state inconsistency between
		// the share registry and the local-store map — log at Error per
		// CLAUDE.md rule 6 (unexpected errors).
		logger.Error("migrate-status: local store dir lookup failed",
			"share", share, "error", err)
		InternalServerError(w, "local store dir lookup: "+err.Error())
		return
	}

	// 3. Read journal + snapshot (if any). Read-only open so a
	//    concurrent migration writer (fail-loud + offline-only, but
	//    keep the read side robust anyway) cannot be interfered with
	//    — OpenJournalReadOnly never truncates or rotates.
	if journalDir != "" {
		j, jerr := migrate.OpenJournalReadOnly(journalDir)
		if jerr != nil {
			// Don't fail the request — journal-read errors are
			// operationally distinct from "share not found"; log and
			// surface the live BlockLayout + zero counters.
			logger.Warn("migrate-status: journal read failed",
				"share", share, "dir", journalDir, "error", jerr)
		} else {
			defer func() { _ = j.Close() }()
			entries, jPresent, sPresent, lastCommit := j.Aggregate()
			resp.JournalPresent = jPresent
			resp.SnapshotPresent = sPresent
			resp.FilesDone = len(entries)
			if !lastCommit.IsZero() {
				resp.LastCommitAt = lastCommit.UTC().Format(time.RFC3339)
			}
			for _, e := range entries {
				resp.BytesUploaded += e.BytesUploaded
				resp.BytesDeduped += e.BytesDeduped
				if e.Kind == "file_skipped" {
					resp.FilesSkipped++
				}
			}
		}
	}

	// 4. FilesTotal: walk the share via migrate.WalkShareFiles —
	//    walk the metadata store directly via the migrate helper.
	//    Bounded at fileWalkTimeout so a TB-scale share cannot hold
	//    the API server hostage. On timeout or error, FilesTotal is
	//    set to -1 (incomplete sentinel) and the rest of the response
	//    still ships.
	if r.URL.Query().Get("with_total") != "false" {
		ctx, cancel := context.WithTimeout(r.Context(), fileWalkTimeout)
		defer cancel()
		total := 0
		walkErr := migrate.WalkShareFiles(ctx, mds, share,
			func(_ metadata.FileHandle, _ *metadata.File) error {
				total++
				return nil
			})
		if walkErr != nil {
			logger.Warn("migrate-status: file walk failed/incomplete",
				"share", share, "error", walkErr, "partial_total", total)
			resp.FilesTotal = -1
		} else {
			resp.FilesTotal = total
		}
	}

	WriteJSONOK(w, resp)
}
