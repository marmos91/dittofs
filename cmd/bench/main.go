// Command dfsbench is the DittoFS-vs-competitors benchmark harness.
//
// The fio-based comparison, provisioning, and reporting commands land in
// follow-up PRs (see issue #1602). This binary currently establishes the
// entrypoint and carries the salvaged SSH executor (exec.go) the provisioning
// path will drive; running it with no subcommand prints help.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:           "dfsbench",
		Short:         "DittoFS benchmark harness (fio comparison — under construction, see #1602)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
