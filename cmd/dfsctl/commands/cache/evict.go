package cache

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var evictCmd = &cobra.Command{
	Use:   "evict",
	Short: "Evict cached data",
	Long: `Evict block store cache data.

By default, evicts both L1 read cache and local disk cache for all shares.
Use --l1-only to evict only the L1 read cache (in-memory).
Use --local-only to evict only local disk cache (preserves L1).
Use --share to evict a specific share only.

Safety: Cache eviction of local blocks is refused if no remote store is
configured for a share, since that would cause data loss.

Examples:
  # Evict all cache tiers for all shares
  dfsctl cache evict

  # Evict only L1 read cache
  dfsctl cache evict --l1-only

  # Evict only local disk cache
  dfsctl cache evict --local-only

  # Evict cache for a specific share
  dfsctl cache evict --share /export

  # Verbose output
  dfsctl cache evict -v`,
	RunE: runCacheEvict,
}

func init() {
	evictCmd.Flags().String("share", "", "Evict cache for a specific share only")
	evictCmd.Flags().Bool("l1-only", false, "Evict only L1 read cache (in-memory)")
	evictCmd.Flags().Bool("local-only", false, "Evict only local disk cache (preserves L1)")
}

func runCacheEvict(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	shareName, _ := cmd.Flags().GetString("share")
	l1Only, _ := cmd.Flags().GetBool("l1-only")
	localOnly, _ := cmd.Flags().GetBool("local-only")

	req := &apiclient.CacheEvictRequest{
		L1Only:    l1Only,
		LocalOnly: localOnly,
	}

	var resp *apiclient.CacheEvictResult
	if shareName != "" {
		resp, err = client.CacheEvictForShare(shareName, req)
	} else {
		resp, err = client.CacheEvict(req)
	}
	if err != nil {
		return fmt.Errorf("failed to evict cache: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, resp)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, resp)
	default:
		if cmdutil.IsVerbose() {
			fmt.Printf("Evicted %d blocks (%s freed), L1 entries cleared: %d\n",
				resp.LocalBlocksEvicted,
				formatBytes(resp.BytesFreed),
				resp.L1EntriesCleared,
			)
		} else {
			cmdutil.PrintSuccess("Cache evicted successfully")
		}
	}

	return nil
}
