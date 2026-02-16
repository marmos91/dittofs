package share

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
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

// Note: listMountsCmd is added in share.go init()

// MountInfo represents information about a mounted share
type MountInfo struct {
	Source     string
	MountPoint string
	Protocol   string
	Options    string
}

func runListMounts(cmd *cobra.Command, args []string) error {
	// Validate platform
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("unsupported platform: %s\nHint: Supported platforms are macOS and Linux", runtime.GOOS)
	}

	// Get optional share filter
	var shareFilter string
	if len(args) > 0 {
		shareFilter = args[0]
		// Normalize share filter - ensure it starts with /
		if !strings.HasPrefix(shareFilter, "/") {
			shareFilter = "/" + shareFilter
		}
	}

	mounts, err := getDittoFSMounts()
	if err != nil {
		return err
	}

	// Filter by share if specified
	if shareFilter != "" {
		filtered := make([]MountInfo, 0)
		for _, m := range mounts {
			// Check if source contains the share path
			// Source format: localhost:/share or //localhost/share
			if strings.Contains(m.Source, ":"+shareFilter) || strings.HasSuffix(m.Source, shareFilter) {
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

func getDittoFSMounts() ([]MountInfo, error) {
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list mounts: %w", err)
	}

	var mounts []MountInfo
	lines := strings.Split(string(output), "\n")

	// Patterns for DittoFS mounts (NFS and SMB from localhost).
	//
	// These regular expressions parse the output of the `mount` command, which differs
	// by platform. They assume the following output formats:
	//
	//   macOS/BSD (no explicit "type" keyword):
	//     NFS: localhost:/export        on /private/tmp/nfs (nfs, ...)
	//     SMB: //user@localhost/share   on /private/tmp/smb (smbfs, ...)
	//
	//   Linux (explicit "type" keyword):
	//     NFS: localhost:/export        on /mnt/nfs type nfs (...)
	//     SMB: //localhost/share        on /mnt/smb type cifs (...)
	//
	// Key assumptions:
	//   - Source appears at line start, mount point follows " on "
	//   - Filesystem type is in parentheses (macOS) or after "type" (Linux)
	//   - Only mounts from "localhost" are considered DittoFS candidates
	//
	// If mount output format changes across OS versions, these patterns may need updates.

	nfsPattern := regexp.MustCompile(`^localhost:(/\S+)\s+on\s+(\S+)\s+.*\((nfs|type nfs)`)
	smbPattern := regexp.MustCompile(`^//[^@]*@?localhost[:/](\S+)\s+on\s+(\S+)\s+.*\((smbfs|cifs|type cifs)`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check for NFS mount from localhost
		if matches := nfsPattern.FindStringSubmatch(line); matches != nil {
			mounts = append(mounts, MountInfo{
				Source:     "localhost:" + matches[1],
				MountPoint: matches[2],
				Protocol:   "NFS",
			})
			continue
		}

		// Check for SMB mount from localhost
		if matches := smbPattern.FindStringSubmatch(line); matches != nil {
			mounts = append(mounts, MountInfo{
				Source:     "//localhost/" + matches[1],
				MountPoint: matches[2],
				Protocol:   "SMB",
			})
			continue
		}

		// Also check for direct localhost mounts (without protocol prefix detection)
		if strings.Contains(line, "localhost") && (strings.Contains(line, "(nfs") || strings.Contains(line, "type nfs")) {
			parts := strings.Fields(line)
			if len(parts) >= 3 && parts[1] == "on" {
				mounts = append(mounts, MountInfo{
					Source:     parts[0],
					MountPoint: parts[2],
					Protocol:   "NFS",
				})
			}
		}
	}

	return mounts, nil
}
