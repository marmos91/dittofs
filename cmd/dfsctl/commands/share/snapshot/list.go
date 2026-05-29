package snapshot

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	listState      string
	listNamePrefix string
	listNoRelative bool
)

var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List snapshots for a share",
	Long: `List snapshots for a share, newest-first.

Examples:
  # List as table
  dfsctl share snapshot list /archive

  # Filter by state
  dfsctl share snapshot list /archive --state ready

  # Filter by name prefix
  dfsctl share snapshot list /archive --name-prefix weekly

  # JSON output
  dfsctl share snapshot list /archive -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVar(&listState, "state", "", "Filter by state (creating|ready|failed|restoring)")
	listCmd.Flags().StringVar(&listNamePrefix, "name-prefix", "", "Filter by name prefix")
	listCmd.Flags().BoolVar(&listNoRelative, "no-relative", false, "Print absolute timestamps instead of relative")
}

// snapshotRow renders one row of the snapshot list table.
type snapshotRow struct {
	ID         string
	Name       string
	State      string
	Durable    string
	Created    string
	Size       string
	noRelative bool
}

// SnapshotList renders a slice of snapshots as a 6-column table.
type SnapshotList []snapshotRow

// Headers implements TableRenderer.
func (sl SnapshotList) Headers() []string {
	return []string{"ID", "NAME", "STATE", "DURABLE", "CREATED", "SIZE"}
}

// Rows implements TableRenderer.
func (sl SnapshotList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, r := range sl {
		rows = append(rows, []string{r.ID, r.Name, r.State, r.Durable, r.Created, r.Size})
	}
	return rows
}

// truncID truncates a snapshot ID to 8 characters for column display.
func truncID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// formatCreated returns either a relative ("3h ago") or absolute string.
func formatCreated(t time.Time, noRelative bool) string {
	if t.IsZero() {
		return "-"
	}
	if noRelative {
		return t.UTC().Format(time.RFC3339)
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// formatSize renders DumpBytes for the list view. In list mode the
// handler does not populate ManifestCount, so we render DumpBytes if
// present and "-" otherwise.
func formatSize(snap apiclient.Snapshot) string {
	if snap.DumpBytes <= 0 {
		return "-"
	}
	return bytesize.ByteSize(snap.DumpBytes).String()
}

// applyFilters returns only snapshots matching the configured filters,
// sorted newest-first.
func applyFilters(snaps []apiclient.Snapshot, state, namePrefix string) []apiclient.Snapshot {
	out := make([]apiclient.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if state != "" && s.State != state {
			continue
		}
		if namePrefix != "" && !strings.HasPrefix(s.Name, namePrefix) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func runList(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	snaps, err := client.ListSnapshots(share)
	if err != nil {
		return fmt.Errorf("failed to list snapshots: %w", err)
	}

	snaps = applyFilters(snaps, listState, listNamePrefix)

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, snaps)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, snaps)
	default:
		rows := make(SnapshotList, 0, len(snaps))
		for _, s := range snaps {
			rows = append(rows, snapshotRow{
				ID:      truncID(s.ID),
				Name:    cmdutil.EmptyOr(s.Name, "-"),
				State:   s.State,
				Durable: cmdutil.BoolToYesNo(s.RemoteDurable),
				Created: formatCreated(s.CreatedAt, listNoRelative),
				Size:    formatSize(s),
			})
		}
		if len(rows) == 0 {
			fmt.Printf("No snapshots on share %q.\n", share)
			return nil
		}
		return output.PrintTable(os.Stdout, rows)
	}
}
