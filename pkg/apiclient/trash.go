package apiclient

import (
	"fmt"
	"net/url"
	"time"
)

// TrashEntry is one recycled root in a share's recycle bin, mirroring the
// server-side trash.Entry wire shape (#190).
type TrashEntry struct {
	// BinPath identifies the entry's path under #recycle.
	BinPath string `json:"bin_path"`
	// OriginalPath is the share-relative path the node occupied before deletion.
	OriginalPath string `json:"original_path"`
	// DeletedBy is the principal that recycled the node (display only).
	DeletedBy string `json:"deleted_by"`
	// DeletedAt is when the node was recycled.
	DeletedAt time.Time `json:"deleted_at"`
	// Size is the file size in bytes (0 for directories).
	Size uint64 `json:"size"`
	// IsDir reports whether the entry is a directory subtree.
	IsDir bool `json:"is_dir"`
}

// TrashStatus is the recycle-bin roll-up for a share, mirroring the
// server-side trash.Status wire shape (#190).
type TrashStatus struct {
	// Enabled reports whether the share's trash policy is enabled.
	Enabled bool `json:"enabled"`
	// ItemCount is the number of recycled roots in the bin.
	ItemCount int `json:"item_count"`
	// TotalBytes is the summed Size of every recycled root.
	TotalBytes uint64 `json:"total_bytes"`
	// Oldest is the earliest DeletedAt across the bin, nil when empty.
	Oldest *time.Time `json:"oldest,omitempty"`
}

// trashRestoreRequest is the POST /trash/restore body.
type trashRestoreRequest struct {
	BinPath string `json:"bin_path"`
	To      string `json:"to"`
}

// trashEmptyRequest is the POST /trash/empty body.
type trashEmptyRequest struct {
	Force bool `json:"force"`
}

// trashEmptyResponse decodes the POST /trash/empty result.
type trashEmptyResponse struct {
	Removed int `json:"removed"`
}

// TrashList returns the recycle-bin entries for a share. The server emits an
// empty array (never null) when the bin is empty.
func (c *Client) TrashList(share string) ([]TrashEntry, error) {
	return listResources[TrashEntry](
		c,
		fmt.Sprintf("/api/v1/shares/%s/trash", url.PathEscape(normalizeShareNameForAPI(share))),
	)
}

// TrashRestore moves the recycled root at binPath back to to (empty restores
// to the entry's recorded original path). Maps a 409 to a conflict APIError
// (destination occupied) and a 404 to a not-found APIError.
func (c *Client) TrashRestore(share, binPath, to string) error {
	return c.post(
		fmt.Sprintf("/api/v1/shares/%s/trash/restore", url.PathEscape(normalizeShareNameForAPI(share))),
		trashRestoreRequest{BinPath: binPath, To: to},
		nil,
	)
}

// TrashEmpty purges the share's recycle bin and returns the number of recycled
// roots removed. force is advisory at the service layer.
func (c *Client) TrashEmpty(share string, force bool) (int, error) {
	var resp trashEmptyResponse
	if err := c.post(
		fmt.Sprintf("/api/v1/shares/%s/trash/empty", url.PathEscape(normalizeShareNameForAPI(share))),
		trashEmptyRequest{Force: force},
		&resp,
	); err != nil {
		return 0, err
	}
	return resp.Removed, nil
}

// TrashStatus returns the recycle-bin roll-up (enabled, item count, total
// bytes, oldest deletion) for a share.
func (c *Client) TrashStatus(share string) (*TrashStatus, error) {
	return getResource[TrashStatus](
		c,
		fmt.Sprintf("/api/v1/shares/%s/trash/status", url.PathEscape(normalizeShareNameForAPI(share))),
	)
}
