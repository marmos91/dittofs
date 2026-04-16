//go:build linux

package fs

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// detectFilesystemType returns the filesystem type (e.g. "ext4", "nfs4",
// "fuse.sshfs") for the filesystem containing path. Best-effort: returns
// "" on any error. Used only for the D-14 remote-FS warning — never for
// gating behavior.
//
// Implementation parses /proc/mounts and picks the longest mount-point
// prefix that matches the absolute path. /proc/mounts fields are
// space-separated: <source> <mount-point> <fstype> <options> <freq> <pass>.
func detectFilesystemType(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var best, bestType string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 3 {
			continue
		}
		mp := parts[1]
		// abs must equal mp, or abs must sit strictly under mp/. Avoid
		// matching /foo/bar against a mount-point /foo/b (prefix would
		// pass without the trailing separator check).
		if abs != mp && !strings.HasPrefix(abs+"/", mp+"/") {
			continue
		}
		if len(mp) > len(best) {
			best = mp
			bestType = parts[2]
		}
	}
	return bestType
}
