package shares

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ShareJournalDir returns where a share's local storage lives beneath the
// server's journal root.
//
// The share name is sanitized because it arrives from an operator and reaches
// the filesystem here; the layout keeps the historical `shares/<name>` level so
// a root adopted from an existing install addresses the same directories it
// always did.
func ShareJournalDir(root, shareName string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, "shares", sanitizeShareName(shareName))
}

// RemoveShareJournal deletes a share's directory beneath the server's journal
// root and everything kept in it.
func RemoveShareJournal(shareName string, defaults *LocalStoreDefaults) error {
	// Without a root no share has a journal to remove.
	if defaults == nil || defaults.JournalRoot == "" {
		return nil
	}
	shareDir := ShareJournalDir(defaults.JournalRoot, shareName)
	if err := checkUnderJournalRoot(defaults.JournalRoot, shareDir); err != nil {
		return err
	}
	return os.RemoveAll(shareDir)
}

// checkUnderJournalRoot confirms a share directory sits strictly beneath the
// journal root before anything is created there: the share name arrives from an
// operator and reaches the filesystem through this directory, so containment is
// established at the sink rather than inferred from the name having been
// escaped.
//
// The root itself is outside, not inside: a journal written there would sit
// beside the directory that keeps shares apart instead of within it.
func checkUnderJournalRoot(root, shareDir string) error {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(shareDir))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("share directory %q escapes the journal root %q", shareDir, root)
	}
	return nil
}
