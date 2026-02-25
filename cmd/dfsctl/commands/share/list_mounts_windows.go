//go:build windows

package share

import (
	"fmt"
	"os/exec"
	"strings"
)

func getDittoFSMounts() ([]MountInfo, error) {
	cmd := exec.Command("net", "use")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list mounts: %w", err)
	}

	var mounts []MountInfo
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), "localhost") {
			continue
		}
		// net use output format: "Status  Local  Remote  Network"
		// e.g.: "OK           Z:        \\localhost\export        Microsoft Windows Network"
		// Fields are separated by whitespace but positions vary.
		// Look for the field containing \\localhost as the remote, and the drive letter as local.
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		var local, remote string
		for _, f := range fields {
			if strings.Contains(strings.ToLower(f), `\\localhost`) {
				remote = f
			}
			if len(f) == 2 && f[1] == ':' && f[0] >= 'A' && f[0] <= 'Z' {
				local = f
			}
		}
		if remote == "" {
			continue
		}

		protocol := "SMB"
		if strings.Contains(strings.ToLower(line), "nfs") {
			protocol = "NFS"
		}
		mounts = append(mounts, MountInfo{
			Source:     remote,
			MountPoint: local,
			Protocol:   protocol,
		})
	}
	return mounts, nil
}
