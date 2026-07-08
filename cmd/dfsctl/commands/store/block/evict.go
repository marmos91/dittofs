package block

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
	Short: "Evict block store data",
	Long: `Evict block store data from local storage, forcing subsequent reads
to fetch from the remote (S3) tier.

By default, evicts both the in-memory read buffer and the resident local
disk blocks for all shares. Local eviction drains every locally-resident
block whose bytes are already synced to the remote — including the sealed
log blobs that hold the bulk of resident data after a rollup, which the
lazy --local-store-size cap only reclaims on the write path. Blocks not yet
uploaded to the remote are never dropped.

Use --read-buffer-only to evict only the read buffer (in-memory).
Use --local-only to evict only local disk data (preserves read buffer).
Use --share to evict a specific share only.

Safety: eviction of local blocks is refused if no remote store is
configured for a share, since that would cause data loss.

Uses: reclaim local disk on demand, or force cold (remote-served) reads for
read-path benchmarking — the local tier is otherwise sticky, so a benchmark
would measure locally-served reads.

Examples:
  # Evict all storage tiers for all shares (drops resident local blocks)
  dfsctl store block evict

  # Evict only read buffer
  dfsctl store block evict --read-buffer-only

  # Evict only local disk data
  dfsctl store block evict --local-only

  # Evict data for a specific share
  dfsctl store block evict --share /export

  # Verbose output
  dfsctl store block evict -v`,
	RunE: runBlockStoreEvict,
}

func init() {
	evictCmd.Flags().String("share", "", "Evict data for a specific share only")
	evictCmd.Flags().Bool("read-buffer-only", false, "Evict only read buffer (in-memory)")
	evictCmd.Flags().Bool("local-only", false, "Evict only local disk data (preserves read buffer)")
}

func runBlockStoreEvict(cmd *cobra.Command, _ []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	shareName, _ := cmd.Flags().GetString("share")
	readBufferOnly, _ := cmd.Flags().GetBool("read-buffer-only")
	localOnly, _ := cmd.Flags().GetBool("local-only")

	req := &apiclient.BlockStoreEvictOptions{
		ReadBufferOnly: readBufferOnly,
		LocalOnly:      localOnly,
	}

	var resp *apiclient.BlockStoreEvictResult
	if shareName != "" {
		resp, err = client.BlockStoreEvictForShare(shareName, req)
	} else {
		resp, err = client.BlockStoreEvict(req)
	}
	if err != nil {
		return fmt.Errorf("failed to evict block store data: %w", err)
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
			fmt.Printf("Evicted %d files (%s freed), read buffer entries cleared: %d\n",
				resp.LocalFilesEvicted,
				formatBytes(resp.BytesFreed),
				resp.ReadBufferEntriesCleared,
			)
		} else {
			cmdutil.PrintSuccess("Block store data evicted successfully")
		}
	}

	return nil
}
