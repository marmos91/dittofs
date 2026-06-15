package quota

import (
	"fmt"
	"os"
	"strconv"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List all quotas on a share",
	Long: `List all per-identity quotas configured on a share.

Examples:
  # List quotas as a table
  dfsctl quota list /archive

  # List as JSON
  dfsctl quota list /archive -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runList,
}

// quotaRow holds resolved quota info for table display.
type quotaRow struct {
	Scope      string `json:"scope"`
	ID         string `json:"id"`
	LimitBytes string `json:"limit_bytes"`
	UsedBytes  string `json:"used_bytes"`
	LimitFiles string `json:"limit_files"`
	UsedFiles  string `json:"used_files"`
	Grace      string `json:"grace"`
}

// QuotaList is a list of quotas for table rendering.
type QuotaList []quotaRow

// Headers implements TableRenderer.
func (ql QuotaList) Headers() []string {
	return []string{"SCOPE", "ID", "LIMIT BYTES", "USED BYTES", "LIMIT FILES", "USED FILES", "GRACE"}
}

// Rows implements TableRenderer.
func (ql QuotaList) Rows() [][]string {
	rows := make([][]string, 0, len(ql))
	for _, q := range ql {
		rows = append(rows, []string{q.Scope, q.ID, q.LimitBytes, q.UsedBytes, q.LimitFiles, q.UsedFiles, q.Grace})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	quotas, err := client.ListQuotas(share)
	if err != nil {
		return fmt.Errorf("failed to list quotas: %w", err)
	}

	rows := make(QuotaList, 0, len(quotas))
	for _, q := range quotas {
		id := "-"
		if q.IdentityID != nil {
			id = strconv.FormatUint(uint64(*q.IdentityID), 10)
		}
		limitBytes := "unlimited"
		if q.LimitBytes != "" {
			limitBytes = q.LimitBytes
		}
		limitFiles := "unlimited"
		if q.LimitFiles > 0 {
			limitFiles = strconv.FormatInt(q.LimitFiles, 10)
		}
		grace := "-"
		if q.GraceSeconds > 0 {
			grace = fmt.Sprintf("%ds", q.GraceSeconds)
		}
		rows = append(rows, quotaRow{
			Scope:      q.Scope,
			ID:         id,
			LimitBytes: limitBytes,
			UsedBytes:  bytesize.ByteSize(q.UsedBytes).String(),
			LimitFiles: limitFiles,
			UsedFiles:  strconv.FormatInt(q.UsedFiles, 10),
			Grace:      grace,
		})
	}

	return cmdutil.PrintOutput(os.Stdout, rows, len(rows) == 0, "No quotas found.", rows)
}
