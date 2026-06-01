package trash

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// listCmd prints the recycle-bin entries for a share.
var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List recycle-bin entries for a share",
	Long: `List the recycled roots in a share's recycle bin.

Each entry shows where it now lives under #recycle, the path it occupied
before deletion, who deleted it, when, its size, and whether it is a
directory subtree.

Examples:
  dfsctl trash list myshare
  dfsctl trash list myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runTrashList,
}

// trashRow holds a recycle-bin entry for table display.
type trashRow struct {
	Path      string `json:"path"`
	Original  string `json:"original"`
	DeletedBy string `json:"deleted_by"`
	DeletedAt string `json:"deleted_at"`
	Size      string `json:"size"`
	Type      string `json:"type"`
}

// TrashList is a list of recycle-bin entries for table rendering.
type TrashList []trashRow

// Headers implements output.TableRenderer.
func (tl TrashList) Headers() []string {
	return []string{"PATH", "ORIGINAL", "DELETED BY", "DELETED AT", "SIZE", "TYPE"}
}

// Rows implements output.TableRenderer.
func (tl TrashList) Rows() [][]string {
	rows := make([][]string, 0, len(tl))
	for _, r := range tl {
		rows = append(rows, []string{r.Path, r.Original, r.DeletedBy, r.DeletedAt, r.Size, r.Type})
	}
	return rows
}

func runTrashList(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	entries, err := client.TrashList(share)
	if err != nil {
		return fmt.Errorf("failed to list trash: %w", err)
	}

	rows := make(TrashList, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, trashRow{
			Path:      e.BinPath,
			Original:  e.OriginalPath,
			DeletedBy: e.DeletedBy,
			DeletedAt: e.DeletedAt.Format("2006-01-02T15:04:05Z07:00"),
			Size:      bytesize.ByteSize(e.Size).String(),
			Type:      entryType(e),
		})
	}

	return cmdutil.PrintOutput(os.Stdout, rows, len(rows) == 0, "Trash is empty.", rows)
}

// entryType maps a recycle-bin entry to a human-readable kind.
func entryType(e apiclient.TrashEntry) string {
	if e.IsDir {
		return "dir"
	}
	return "file"
}
