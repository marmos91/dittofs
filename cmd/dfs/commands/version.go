package commands

import (
	"runtime"

	"github.com/spf13/cobra"
)

var versionShort bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Long:  `Display the DittoFS version, build information, and system details.`,
	Run: func(cmd *cobra.Command, args []string) {
		if versionShort {
			cmd.Println(Version)
			return
		}

		cmd.Printf("%s %s\n", cmd.Root().Name(), Version)
		cmd.Printf("  Commit:     %s\n", Commit)
		cmd.Printf("  Built:      %s\n", Date)
		cmd.Printf("  Go version: %s\n", runtime.Version())
		cmd.Printf("  OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionShort, "short", false, "Show only version number")
}
