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
to fetch from the remote tier.

By default, evicts both the in-memory read buffer and the resident local
disk blocks for all shares. Local eviction drains every locally-resident
block whose bytes are already synced to the remote — including the sealed
log blobs that hold the bulk of resident data after a rollup, which the
lazy --journal-size cap only reclaims on the write path. Blocks not yet
uploaded to the remote are never dropped.

Use --read-buffer-only to evict only the read buffer (in-memory).
Use --local-only to evict only local disk data (preserves read buffer).
Use --share to evict a specific share only.

Safety: blocks that have not yet reached the share's block store are
never dropped, since that would cause data loss. Eviction reclaims whole
segments, so a segment holding even one un-uploaded record stays resident
along with every synced block sharing it — run 'dfsctl system drain-uploads'
until 'dfsctl store block stats' reports 0 pending remote bytes if you need
everything to go cold.

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
			fmt.Printf("Evicted %d segments (%s freed), read buffer entries cleared: %d\n",
				resp.SegmentsEvicted,
				formatBytes(resp.BytesFreed),
				resp.ReadBufferEntriesCleared,
			)
		} else {
			cmdutil.PrintSuccess("Block store data evicted successfully")
		}
		if reason := evictShortfall(resp); reason != "" {
			fmt.Println(reason)
		}
	}

	return nil
}

// evictShortfall explains a local evict that reclaimed nothing, or returns ""
// when there is nothing to explain. Freeing no bytes is a normal outcome with
// more than one cause, and the plain "evicted successfully" line reads as if
// there had been nothing left to free — which is the one thing it does not
// mean. Callers that force a cold read act on that line, so the reason belongs
// next to it rather than in a separate stats command.
func evictShortfall(resp *apiclient.BlockStoreEvictResult) string {
	if resp.BytesFreed > 0 {
		return ""
	}
	switch {
	case resp.EvictionHeld:
		return "Nothing was reclaimed: eviction is held off for this store " +
			"(the remote is unreachable, or the store is pinned by retention policy). " +
			"Local data stays resident until it clears."
	case resp.UnsyncedBytesPinned > 0:
		return fmt.Sprintf("Nothing was reclaimed: %s of local data has not reached the remote yet, "+
			"and eviction keeps any whole segment holding un-uploaded bytes. "+
			"Run `dfsctl system drain-uploads` until `store block stats` reports 0 pending remote bytes, then evict again.",
			formatBytes(resp.UnsyncedBytesPinned))
	default:
		return ""
	}
}
