package commands

import (
	"fmt"
	"net/url"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var switchUserPassword string

var switchUserCmd = &cobra.Command{
	Use:   "switch-user <username>",
	Short: "Switch to a different user on the current server",
	Long: `Switch to a different user on the current server.

This command authenticates as the specified user against the same server
configured in the current context, creating a new context if needed.

If a context already exists for this user on the same server and has a valid
(non-expired) token, it switches to that context without re-authenticating.

Examples:
  # Switch to user marmos91 (will prompt for password)
  dfsctl switch-user marmos91

  # Switch with password on command line
  dfsctl switch-user marmos91 -p secret`,
	Args: cobra.ExactArgs(1),
	RunE: runSwitchUser,
}

func init() {
	switchUserCmd.Flags().StringVarP(&switchUserPassword, "password", "p", "", "Password (will prompt if not provided)")
}

func runSwitchUser(cmd *cobra.Command, args []string) error {
	username := args[0]

	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Get server URL from current context
	currentCtx, err := store.GetCurrentContext()
	if err != nil {
		return fmt.Errorf("no current context - login first with: dfsctl login --server <url>")
	}

	serverURL := currentCtx.ServerURL

	// Derive context name: username@host
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Errorf("invalid server URL in current context: %w", err)
	}
	contextName := fmt.Sprintf("%s@%s", username, parsed.Host)

	// Check if a context for this user+server already exists with valid token
	if existingCtx, err := store.GetContext(contextName); err == nil {
		if existingCtx.ServerURL == serverURL && existingCtx.Username == username && !existingCtx.IsExpired() {
			if err := store.UseContext(contextName); err != nil {
				return fmt.Errorf("failed to switch context: %w", err)
			}
			fmt.Printf("Switched to user %s (context: %s)\n", username, contextName)
			return nil
		}
	}

	// Need to authenticate - get password
	password := switchUserPassword
	if password == "" {
		password, err = prompt.Password(fmt.Sprintf("Password for %s", username))
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// Authenticate
	client := apiclient.New(serverURL)
	fmt.Printf("Authenticating as %s on %s...\n", username, serverURL)

	tokens, err := client.Login(username, password)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Save context
	ctx := &credentials.Context{
		ServerURL:    serverURL,
		Username:     username,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    tokens.ExpiresAt,
	}

	if err := store.SetContext(contextName, ctx); err != nil {
		return fmt.Errorf("failed to save credentials: %w", err)
	}

	if err := store.UseContext(contextName); err != nil {
		return fmt.Errorf("failed to switch context: %w", err)
	}

	fmt.Printf("Switched to user %s (context: %s)\n", username, contextName)
	return nil
}
