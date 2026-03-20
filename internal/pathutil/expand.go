package pathutil

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath expands a leading ~ in a file path to the user's home directory.
// If the path is exactly "~" or starts with "~/", the ~ is replaced with the
// home directory. All other paths are returned unchanged.
func ExpandPath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, path[1:]), nil
}
