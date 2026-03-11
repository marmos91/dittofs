// Package cache implements the cache management commands for dfsctl.
package cache

import "github.com/spf13/cobra"

// Cmd is the root command for cache management.
var Cmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage block store cache",
	Long: `Manage per-share block store cache (L1 read cache and local disk cache).

Cache commands allow you to inspect cache statistics and evict cached data.
These operations require admin privileges.

Examples:
  # Show aggregated cache statistics
  dfsctl cache stats

  # Show cache stats for a specific share
  dfsctl cache stats --share /export

  # Evict all cache tiers (L1 + local) for all shares
  dfsctl cache evict

  # Evict only L1 read cache
  dfsctl cache evict --l1-only

  # Evict only local disk cache
  dfsctl cache evict --local-only

  # Evict cache for a specific share
  dfsctl cache evict --share /export`,
}

func init() {
	Cmd.AddCommand(statsCmd)
	Cmd.AddCommand(evictCmd)
}
