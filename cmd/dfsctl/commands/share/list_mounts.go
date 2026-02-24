package share

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var listMountsCmd = &cobra.Command{
	Use:     "list-mounts [share]",
	Aliases: []string{"lm", "mounts"},
	Short:   "List mounted DittoFS shares",
	Long: `List all currently mounted DittoFS shares.

This command shows NFS and SMB mounts from localhost that are likely DittoFS shares.
Optionally filter by share name.

Examples:
  # List all mounted DittoFS shares
  dfsctl share list-mounts

  # Filter by share name
  dfsctl share list-mounts /export

  # Short alias
  dfsctl share mounts`,
	Args: cobra.MaximumNArgs(1),
	RunE: runListMounts,
}

// MountInfo represents information about a mounted share.
type MountInfo struct {
	Source     string
	MountPoint string
	Protocol   string
}

func runListMounts(cmd *cobra.Command, args []string) error {
	var shareFilter string
	if len(args) > 0 {
		shareFilter = args[0]
		if !strings.HasPrefix(shareFilter, "/") {
			shareFilter = "/" + shareFilter
		}
	}

	mounts, err := getDittoFSMounts()
	if err != nil {
		return err
	}

	if shareFilter != "" {
		var filtered []MountInfo
		for _, m := range mounts {
			if strings.Contains(m.Source, ":"+shareFilter) ||
				strings.HasSuffix(m.Source, shareFilter) ||
				strings.HasSuffix(m.Source, strings.ReplaceAll(shareFilter, "/", "\\")) {
				filtered = append(filtered, m)
			}
		}
		mounts = filtered
	}

	if len(mounts) == 0 {
		if shareFilter != "" {
			fmt.Printf("No mounts found for share %s\n", shareFilter)
		} else {
			fmt.Println("No DittoFS shares currently mounted")
		}
		return nil
	}

	fmt.Printf("%-10s %-30s %s\n", "PROTOCOL", "SOURCE", "MOUNTPOINT")
	fmt.Printf("%-10s %-30s %s\n", "--------", "------", "----------")
	for _, m := range mounts {
		fmt.Printf("%-10s %-30s %s\n", m.Protocol, m.Source, m.MountPoint)
	}

	return nil
}
