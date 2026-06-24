package commands

import (
	"os"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion script",
	Long: `Generate shell completion script for dfsctl.

The generated script enables tab-completion for dfsctl commands, subcommands,
and flags in your shell. Pick the snippet for your shell and load it once.

Examples:
  # Bash (Linux): install system-wide
  dfsctl completion bash > /etc/bash_completion.d/dfsctl

  # Bash (macOS, requires the Homebrew bash-completion package)
  dfsctl completion bash > $(brew --prefix)/etc/bash_completion.d/dfsctl

  # Zsh: enable completion once (if not already enabled), then install
  echo "autoload -U compinit; compinit" >> ~/.zshrc
  dfsctl completion zsh > "${fpath[1]}/_dfsctl"

  # Zsh (macOS, Homebrew)
  dfsctl completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfsctl

  # Fish
  dfsctl completion fish > ~/.config/fish/completions/dfsctl.fish

  # PowerShell: load for the current session
  dfsctl completion powershell | Out-String | Invoke-Expression

  # PowerShell: persist across sessions by sourcing from your profile
  dfsctl completion powershell > dfsctl.ps1`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return cmd.Root().GenBashCompletion(os.Stdout)
		case "zsh":
			return cmd.Root().GenZshCompletion(os.Stdout)
		case "fish":
			return cmd.Root().GenFishCompletion(os.Stdout, true)
		case "powershell":
			return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
		}
		return nil
	},
}
