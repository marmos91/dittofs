package share

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// shareEnabledString renders the Share.Enabled field as "yes" / "no" for
// human-readable table output.
func shareEnabledString(enabled bool) string {
	if enabled {
		return "yes"
	}
	return "no"
}

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
		{"Enabled", shareEnabledString(s.Enabled)},
		{"Default Permission", s.DefaultPermission},
		{"ACL Canonicalize Inherited", fmt.Sprintf("%v", s.AclFlagInheritedCanonicalization)},
		{"Access-Based Enumeration", fmt.Sprintf("%v", s.AccessBasedEnumeration)},
		{"Change Notify Disabled", fmt.Sprintf("%v", s.ChangeNotifyDisabled)},
		{"Streams Disabled", fmt.Sprintf("%v", s.StreamsDisabled)},
		{"Continuous Availability", fmt.Sprintf("%v", s.ContinuousAvailability)},
		{"Retention", retPolicy},
	}

	// Only show Retention TTL when a TTL is set
	if s.RetentionTTL != "" {
		rows = append(rows, []string{"Retention TTL", s.RetentionTTL})
	}

	// Only show cache size overrides when set
	if s.LocalStoreSize != "" {
		rows = append(rows, []string{"Local Store Size", s.LocalStoreSize})
	}
	if s.ReadBufferSize != "" {
		rows = append(rows, []string{"Read Buffer Size", s.ReadBufferSize})
	}

	// Recycle-bin policy (#190). Always show the enabled state; only show
	// the detail rows when trash is enabled to keep the output clean.
	rows = append(rows, []string{"Trash Enabled", fmt.Sprintf("%v", s.TrashEnabled)})
	if s.TrashEnabled {
		retention := "keep forever"
		if s.TrashRetentionDays > 0 {
			retention = fmt.Sprintf("%d", s.TrashRetentionDays)
		}
		maxSize := "unbounded"
		if s.TrashMaxBytes > 0 {
			maxSize = bytesize.ByteSize(s.TrashMaxBytes).String()
		}
		exclude := "-"
		if len(s.TrashExcludePatterns) > 0 {
			exclude = strings.Join(s.TrashExcludePatterns, ", ")
		}
		rows = append(rows,
			[]string{"Trash Retention (days)", retention},
			[]string{"Trash Restrict Empty To Admin", fmt.Sprintf("%v", s.TrashRestrictToAdmin)},
			[]string{"Trash Max Size", maxSize},
			[]string{"Trash Exclude", exclude},
		)
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
