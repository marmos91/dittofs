package commands

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionShort bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Long: `Display the dfsctl build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

Examples:
  # Show full version information
  dfsctl version

  # Print only the version number (useful in scripts)
  dfsctl version --short`,
	Run: func(cmd *cobra.Command, args []string) {
		if versionShort {
			fmt.Println(Version)
			return
		}

		fmt.Printf("dfsctl %s\n", Version)
		fmt.Printf("  Commit:     %s\n", Commit)
		fmt.Printf("  Built:      %s\n", Date)
		fmt.Printf("  Go version: %s\n", runtime.Version())
		fmt.Printf("  OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionShort, "short", false, "Show only version number")
}
