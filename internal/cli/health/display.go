package health

import (
	"fmt"
	"time"
)

// PrintStorageHealth prints per-share storage health to stdout.
// This is shared between the dfs and dfsctl status commands.
func PrintStorageHealth(sh *StorageHealth) {
	if sh == nil || len(sh.Shares) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("  Storage:")

	offlineCount := 0
	for _, share := range sh.Shares {
		if share.RemoteHealthy {
			fmt.Printf("    %s \033[32m[remote: healthy]\033[0m\n", share.Name)
		} else {
			offlineCount++
			dur := time.Duration(share.OutageDurationSec * float64(time.Second))
			fmt.Printf("    %s \033[33m[remote: offline %s, %d pending]\033[0m\n",
				share.Name,
				dur.Truncate(time.Second),
				share.PendingUploads)
		}
	}

	if offlineCount > 0 {
		fmt.Printf("    \033[33m%d/%d shares offline, %d blocks pending sync\033[0m\n",
			offlineCount, len(sh.Shares), sh.TotalPending)
	}
}
