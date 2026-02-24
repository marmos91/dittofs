//go:build !windows

package share

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Patterns for DittoFS mounts (NFS and SMB from localhost).
//
// These regular expressions parse the output of the `mount` command, which differs
// by platform. They assume the following output formats:
//
//	macOS/BSD (no explicit "type" keyword):
//	  NFS: localhost:/export        on /private/tmp/nfs (nfs, ...)
//	  SMB: //user@localhost/share   on /private/tmp/smb (smbfs, ...)
//
//	Linux (explicit "type" keyword):
//	  NFS: localhost:/export        on /mnt/nfs type nfs (...)
//	  SMB: //localhost/share        on /mnt/smb type cifs (...)
//
// Key assumptions:
//   - Source appears at line start, mount point follows " on "
//   - Filesystem type is in parentheses (macOS) or after "type" (Linux)
//   - Only mounts from "localhost" are considered DittoFS candidates
//
// If mount output format changes across OS versions, these patterns may need updates.
var (
	nfsPattern = regexp.MustCompile(`^localhost:(/\S+)\s+on\s+(\S+)\s+.*\((nfs|type nfs)`)
	smbPattern = regexp.MustCompile(`^//[^@]*@?localhost[:/](\S+)\s+on\s+(\S+)\s+.*\((smbfs|cifs|type cifs)`)
)

func getDittoFSMounts() ([]MountInfo, error) {
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list mounts: %w", err)
	}

	var mounts []MountInfo

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "localhost") {
			continue
		}

		if matches := nfsPattern.FindStringSubmatch(line); matches != nil {
			mounts = append(mounts, MountInfo{
				Source:     "localhost:" + matches[1],
				MountPoint: matches[2],
				Protocol:   "NFS",
			})
			continue
		}

		if matches := smbPattern.FindStringSubmatch(line); matches != nil {
			mounts = append(mounts, MountInfo{
				Source:     "//localhost/" + matches[1],
				MountPoint: matches[2],
				Protocol:   "SMB",
			})
			continue
		}

		// Fallback: catch localhost NFS mounts that don't match the strict regex
		// (e.g., unusual mount output formatting on some distros)
		if strings.Contains(line, "(nfs") || strings.Contains(line, "type nfs") {
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
