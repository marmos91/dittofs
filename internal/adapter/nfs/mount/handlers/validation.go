package handlers

import "fmt"

// ValidateExportPath checks if an export path is valid according to NFS conventions
// This is a helper for consistent validation across mount procedures
func ValidateExportPath(path string) error {
	if path == "" {
		return fmt.Errorf("export path cannot be empty")
	}

	if path[0] != '/' {
		return fmt.Errorf("export path must be absolute (start with /)")
	}

	if len(path) > 1024 {
		return fmt.Errorf("export path too long (max 1024 characters)")
	}

	return nil
}
