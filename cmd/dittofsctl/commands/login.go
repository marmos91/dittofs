package commands

import (
	"fmt"
	"net/url"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	loginServer   string
	loginUsername string
	loginPassword string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with DittoFS server",
	Long: `Authenticate with a DittoFS server and store credentials.

On first login, you must specify the server URL. Subsequent logins will
use the stored server URL unless overridden.

Examples:
  # First login to a server
  dittofsctl login --server http://localhost:8080 --username admin

  # Login with password on command line (less secure)
  dittofsctl login --server http://localhost:8080 -u admin -p secret

  # Re-login to stored server
  dittofsctl login`,
	RunE: runLogin,
}

func init() {
	loginCmd.Flags().StringVar(&loginServer, "server", "", "Server URL (required on first login)")
	loginCmd.Flags().StringVarP(&loginUsername, "username", "u", "", "Username")
	loginCmd.Flags().StringVarP(&loginPassword, "password", "p", "", "Password")
}

func runLogin(cmd *cobra.Command, args []string) error {
	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Determine server URL
	serverURLStr := loginServer
	if serverURLStr == "" {
		// Try to get from current context
		ctx, err := store.GetCurrentContext()
		if err != nil || ctx == nil || ctx.ServerURL == "" {
			return fmt.Errorf("no server URL specified and no saved context found\n\n" +
				"Specify server URL:\n" +
				"  dittofsctl login --server http://localhost:8080")
		}
		serverURLStr = ctx.ServerURL
	}

	// Validate server URL
	parsedURL, err := url.Parse(serverURLStr)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "http"
		serverURLStr = parsedURL.String()
	}

	// Get username (prompt if not provided)
	username := loginUsername
	if username == "" {
		username, err = prompt.InputRequired("Username")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// Get password (prompt if not provided)
	password := loginPassword
	if password == "" {
		password, err = prompt.PasswordWithValidation("Password", 8)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// Create API client
	client := apiclient.New(serverURLStr)

	// Attempt login
	fmt.Printf("Logging in to %s as %s...\n", serverURLStr, username)
	tokens, err := client.Login(username, password)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Determine context name
	contextName := store.GetCurrentContextName()
	if contextName == "" {
		contextName = credentials.GenerateContextName(serverURLStr)
	}

	// Save credentials
	ctx := &credentials.Context{
		ServerURL:    serverURLStr,
		Username:     username,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    tokens.ExpiresAt,
	}

	if err := store.SetContext(contextName, ctx); err != nil {
		return fmt.Errorf("failed to save credentials: %w", err)
	}

	if err := store.UseContext(contextName); err != nil {
		return fmt.Errorf("failed to set current context: %w", err)
	}

	fmt.Printf("Logged in successfully as %s\n", username)
	fmt.Printf("Context: %s\n", contextName)
	fmt.Printf("Credentials saved to: %s\n", store.ConfigPath())

	return nil
}
