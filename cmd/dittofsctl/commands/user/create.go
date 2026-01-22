package user

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	createUsername string
	createPassword string
	createEmail    string
	createRole     string
	createGroups   string
	createEnabled  bool
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new user",
	Long: `Create a new user on the DittoFS server.

If username or password are not provided via flags, you will be prompted
to enter them interactively.

Examples:
  # Create user interactively
  dittofsctl user create

  # Create user with flags
  dittofsctl user create --username alice --password secret

  # Create admin user
  dittofsctl user create --username admin2 --password secret --role admin

  # Create user with email and groups
  dittofsctl user create --username bob --password secret --email bob@example.com --groups editors,viewers`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVarP(&createUsername, "username", "u", "", "Username (required)")
	createCmd.Flags().StringVarP(&createPassword, "password", "p", "", "Password (prompts if not provided)")
	createCmd.Flags().StringVar(&createEmail, "email", "", "Email address")
	createCmd.Flags().StringVar(&createRole, "role", "user", "Role (user|admin)")
	createCmd.Flags().StringVar(&createGroups, "groups", "", "Comma-separated list of groups")
	createCmd.Flags().BoolVar(&createEnabled, "enabled", true, "Enable account")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	username := createUsername
	if username == "" {
		username, err = prompt.InputRequired("Username")
		if err != nil {
			return fmt.Errorf("failed to read username: %w", err)
		}
	}

	password := createPassword
	if password == "" {
		password, err = prompt.PasswordWithConfirmation("Password", "Confirm password", 8)
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
	}

	req := &apiclient.CreateUserRequest{
		Username: username,
		Password: password,
		Email:    createEmail,
		Role:     createRole,
		Groups:   cmdutil.ParseCommaSeparatedList(createGroups),
		Enabled:  &createEnabled,
	}

	user, err := client.CreateUser(req)
	if err != nil {
		return fmt.Errorf("failed to create user: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, user, fmt.Sprintf("User '%s' created successfully", user.Username))
}
