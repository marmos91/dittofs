package exec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Sh runs a command on the local host (the harness runs on the bench VM), and
// on failure returns the combined output so a broken recipe step is legible.
// Backend recipes shell out to mount/exportfs/apt/systemctl through this.
func Sh(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w\n%s", name, args, err, out.String())
	}
	return nil
}

// DropOSCache flushes and drops the kernel page cache so the next read pass is
// genuinely cold, not served from RAM. Universal across backends (runs after a
// backend's own Evict). Linux/root-only; best-effort — the caller warns on error
// rather than aborting the run.
func DropOSCache(ctx context.Context) error {
	return Sh(ctx, "sh", "-c", "sync && echo 3 > /proc/sys/vm/drop_caches")
}
