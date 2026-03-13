package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show share details",
	Long: `Show detailed information about a share including retention settings.

Examples:
  # Show share details
  dfsctl share show /edge-data

  # Show as JSON
  dfsctl share show /edge-data -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runShow,
}

// ShareDetail wraps a share for detailed table rendering.
type ShareDetail struct {
	share *apiclient.Share
}

// Headers implements TableRenderer.
func (sd ShareDetail) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements TableRenderer.
func (sd ShareDetail) Rows() [][]string {
	s := sd.share

	retPolicy := s.RetentionPolicy
	if retPolicy == "" {
		retPolicy = "lru"
	}

	remoteStore := "-"
	if s.RemoteBlockStoreID != nil && *s.RemoteBlockStoreID != "" {
		remoteStore = *s.RemoteBlockStoreID
	}

	rows := [][]string{
		{"Name", s.Name},
		{"ID", s.ID},
		{"Metadata Store", s.MetadataStoreID},
		{"Local Block Store", s.LocalBlockStoreID},
		{"Remote Block Store", remoteStore},
		{"Read Only", fmt.Sprintf("%v", s.ReadOnly)},
		{"Default Permission", s.DefaultPermission},
		{"Retention", retPolicy},
	}

	// Only show Retention TTL when a TTL is set
	if s.RetentionTTL != "" {
		rows = append(rows, []string{"Retention TTL", s.RetentionTTL})
	}

	rows = append(rows,
		[]string{"Created", s.CreatedAt.Format("2006-01-02 15:04:05")},
		[]string{"Updated", s.UpdatedAt.Format("2006-01-02 15:04:05")},
	)

	return rows
}

func runShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	share, err := client.GetShare(name)
	if err != nil {
		return fmt.Errorf("failed to get share: %w", err)
	}

	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil {
		return fmtErr
	}

	// For JSON/YAML, output the whole share
	if format != output.FormatTable {
		return cmdutil.PrintResource(os.Stdout, share, nil)
	}

	return output.PrintTable(os.Stdout, ShareDetail{share: share})
}
