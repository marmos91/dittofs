//go:build windows

package share

import (
	"fmt"
	"os/exec"
	"strings"
)

func getDittoFSMounts() ([]MountInfo, error) {
	cmd := exec.Command("net", "use")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list mounts: %w", err)
	}

	var mounts []MountInfo
	lines := strings.Split(string(output), "\n")

	// Only entries with \\localhost\ in 'net use' output are considered DittoFS candidates.
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "Status") {
			continue
		}

		if !strings.Contains(strings.ToLower(line), `\\localhost\`) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		// The status field may be empty, so find drive letter and UNC path by pattern.
		var local, remote string
		for _, field := range fields {
			if len(field) == 2 && field[1] == ':' {
				local = field
			}
			if strings.HasPrefix(field, `\\`) {
				remote = field
			}
		}

		if remote == "" {
			continue
		}

		mounts = append(mounts, MountInfo{
			Source:     remote,
			MountPoint: local,
			Protocol:   "SMB",
		})
	}

	return mounts, nil
}
