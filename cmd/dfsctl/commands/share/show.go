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
	Long: `Show a detailed, field-by-field view of a single share.

Unlike 'share list', which shows summary columns, 'share show' displays every
attribute of the share: store IDs, read-only state, ACL settings, retention
policy and TTL, cache size overrides, quota, trash (recycle bin) settings, and
creation/update timestamps. Use this command when debugging a misconfigured
share or before editing it.

Examples:
  # Show all fields for a share
  dfsctl share show /archive

  # Emit the full share record as JSON (useful for scripting or diffing)
  dfsctl share show /archive -o json

  # Emit as YAML
  dfsctl share show /archive -o yaml`,
	Args: cobra.ExactArgs(1),
	RunE: runShow,
}

// ShareDetail wraps a share for detailed table rendering.
type ShareDetail struct {
	share *apiclient.Share
	// metaStoreNames and blockStoreNames are best-effort store ID -> name
	// lookups (built via buildStoreNameMaps). A missing entry falls back to the
	// raw ID. Populated in runShow.
	metaStoreNames  map[string]string
	blockStoreNames map[string]string
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
		remoteStore = resolveStoreName(sd.blockStoreNames, *s.RemoteBlockStoreID)
	}

	rows := [][]string{
		{"Name", s.Name},
		{"ID", s.ID},
		{"Metadata Store", resolveStoreName(sd.metaStoreNames, s.MetadataStoreID)},
		{"Local Block Store", resolveStoreName(sd.blockStoreNames, s.LocalBlockStoreID)},
		{"Remote Block Store", remoteStore},
		{"Read Only", fmt.Sprintf("%v", s.ReadOnly)},
		{"Enabled", shareEnabledString(s.Enabled)},
		{"Default Permission", s.DefaultPermission},
		{"Root Owner", shareOwnerString(s.OwnerUID, s.OwnerGID)},
		{"ACL Canonicalize Inherited", fmt.Sprintf("%v", s.AclFlagInheritedCanonicalization)},
		{"Access-Based Enumeration", fmt.Sprintf("%v", s.AccessBasedEnumeration)},
		{"Change Notify Disabled", fmt.Sprintf("%v", s.ChangeNotifyDisabled)},
		{"Streams Disabled", fmt.Sprintf("%v", s.StreamsDisabled)},
		{"Continuous Availability", fmt.Sprintf("%v", s.ContinuousAvailability)},
		{"Allow MFsymlink", fmt.Sprintf("%v", s.AllowMFsymlink)},
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

	// Resolve store IDs to names so the table shows the store name instead of
	// an opaque ID, matching 'share list'. Best-effort: a lookup failure falls
	// back to the raw ID.
	metaNames, blockNames := buildStoreNameMaps(client)

	return output.PrintTable(os.Stdout, ShareDetail{
		share:           share,
		metaStoreNames:  metaNames,
		blockStoreNames: blockNames,
	})
}

// shareOwnerString renders the persisted root-directory owner (#1534) for the
// detail table. Nil UID means the root is owned by root (the default).
func shareOwnerString(uid, gid *uint32) string {
	if uid == nil {
		return "root (0:0)"
	}
	group := *uid
	if gid != nil {
		group = *gid
	}
	return fmt.Sprintf("%d:%d", *uid, group)
}
