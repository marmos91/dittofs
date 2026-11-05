package config

import (
	"fmt"
	"path/filepath"

	"github.com/cubbit/dittofs/internal/pathutil"
)

// defaultBlockDirName is the subdirectory of the state directory that holds
// every share's state directory when blockstore.journal.path is unset.
const defaultBlockDirName = "blocks"

// BlockstoreConfig is the top-level container for blockstore-related
// tunables; additional layers (remote tier, cache tier) may be added in
// subsequent milestones.
type BlockstoreConfig struct {
	Journal BlockstoreJournalConfig `mapstructure:"journal" yaml:"journal"`
}

// BlockstoreJournalConfig holds the per-share local state root.
type BlockstoreJournalConfig struct {
	// Path is the directory holding every share's state directory (gc-state,
	// snapshots, the migration journal). Each share gets its own subdirectory
	// beneath it, so no two shares write into the same directory. A leading ~
	// is expanded; the result must be absolute so the location can never
	// resolve against the server's working directory. Defaults to
	// <state dir>/blocks when unset.
	Path string `mapstructure:"path" yaml:"path"`
}

// ApplyDefaults fills any zero-valued field with the defaults.
func (c *BlockstoreJournalConfig) ApplyDefaults() {
	if c.Path == "" {
		c.Path = filepath.Join(GetStateDir(), defaultBlockDirName)
	}
	// A ~ that cannot be expanded (no home directory) is left as-is and
	// fails Validate's absolute-path check rather than being silently
	// resolved against the working directory.
	if expanded, err := pathutil.ExpandPath(c.Path); err == nil {
		c.Path = expanded
	}
}

// Validate returns an error if the BlockstoreJournalConfig has invalid
// values. The error message includes the canonical dotted config path so
// operators can pinpoint the offending key in their config file.
func (c *BlockstoreJournalConfig) Validate() error {
	// Empty means "apply the default" and is only reachable before
	// ApplyDefaults; a value the operator did set must be absolute.
	if c.Path != "" && !filepath.IsAbs(c.Path) {
		return fmt.Errorf("blockstore.journal.path must be an absolute directory (got %q)", c.Path)
	}
	return nil
}

// ApplyDefaults fans out defaults to every sub-tier.
func (c *BlockstoreConfig) ApplyDefaults() {
	c.Journal.ApplyDefaults()
}

// Validate fans out validation to every sub-tier.
func (c *BlockstoreConfig) Validate() error {
	return c.Journal.Validate()
}
