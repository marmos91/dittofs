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
// by platform:
//
//	macOS/BSD: localhost:/export on /mnt (nfs, ...)
//	Linux:     localhost:/export on /mnt type nfs (...)
var (
	nfsPattern = regexp.MustCompile(`^localhost:(/\S+)\s+on\s+(\S+)\s+.*(?:\(nfs|type nfs)`)
	smbPattern = regexp.MustCompile(`^//[^@]*@?localhost[:/](\S+)\s+on\s+(\S+)\s+.*(?:\(smbfs|\(cifs|type cifs)`)
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
