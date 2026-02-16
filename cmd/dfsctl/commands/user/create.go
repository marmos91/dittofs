package user

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	createUsername string
	createPassword string
	createEmail    string
	createRole     string
	createUID      uint32
	createHostUID  bool
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
  dfsctl user create

  # Create user with flags
  dfsctl user create --username alice --password secret

  # Create admin user
  dfsctl user create --username admin2 --password secret --role admin

  # Create user with email and groups
  dfsctl user create --username bob --password secret --email bob@example.com --groups editors,viewers

  # Create user with specific UID
  dfsctl user create --username bob --password secret --uid 1001

  # Create user with your current host UID (for NFS access)
  dfsctl user create --username bob --password secret --host-uid`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVarP(&createUsername, "username", "u", "", "Username (required)")
	createCmd.Flags().StringVarP(&createPassword, "password", "p", "", "Password (prompts if not provided)")
	createCmd.Flags().StringVar(&createEmail, "email", "", "Email address")
	createCmd.Flags().StringVar(&createRole, "role", "user", "Role (user|admin)")
	createCmd.Flags().Uint32Var(&createUID, "uid", 0, "Unix user ID (auto-assigned if not specified)")
	createCmd.Flags().BoolVar(&createHostUID, "host-uid", false, "Use current host user's UID (for NFS access)")
	createCmd.Flags().StringVar(&createGroups, "groups", "", "Comma-separated list of groups")
	// MarkFlagsMutuallyExclusive panics if flag names don't exist (see Cobra source)
	createCmd.MarkFlagsMutuallyExclusive("uid", "host-uid")
	createCmd.Flags().BoolVar(&createEnabled, "enabled", true, "Enable account")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Check if running interactively (no flags provided)
	interactive := !cmd.Flags().Changed("username")

	username := createUsername
	if username == "" {
		username, err = prompt.InputRequired("Username")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	password := createPassword
	if password == "" {
		password, err = prompt.PasswordWithConfirmation("Password", "Confirm password", 8)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// Prompt for optional fields if running interactively
	email := createEmail
	if interactive && !cmd.Flags().Changed("email") {
		email, err = prompt.InputOptional("Email")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	role := createRole
	if interactive && !cmd.Flags().Changed("role") {
		role, err = prompt.Select("Role", []prompt.SelectOption{
			{Label: "user", Value: "user", Description: "Regular user with standard permissions"},
			{Label: "admin", Value: "admin", Description: "Administrator with full access"},
		})
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	groups := createGroups
	if interactive && !cmd.Flags().Changed("groups") {
		groups, err = prompt.Input("Groups (comma-separated)", "users")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// UID - use host-uid, explicit uid, or prompt if interactive
	var uid *uint32
	if createHostUID {
		hostUID := os.Getuid()
		if hostUID < 0 {
			return fmt.Errorf("--host-uid is not supported on this platform")
		}
		hostUIDUint32 := uint32(hostUID)
		uid = &hostUIDUint32
	} else if cmd.Flags().Changed("uid") {
		uid = &createUID
	} else if interactive {
		uidInput, err := prompt.InputOptional("UID (leave empty for auto-assign, or use --host-uid)")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		if uidInput != "" {
			var uidVal uint32
			if _, err := fmt.Sscanf(uidInput, "%d", &uidVal); err == nil {
				uid = &uidVal
			}
		}
	}

	req := &apiclient.CreateUserRequest{
		Username: username,
		Password: password,
		Email:    email,
		Role:     role,
		UID:      uid,
		Groups:   cmdutil.ParseCommaSeparatedList(groups),
		Enabled:  &createEnabled,
	}

	user, err := client.CreateUser(req)
	if err != nil {
		return fmt.Errorf("failed to create user: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, user, fmt.Sprintf("User '%s' created successfully", user.Username))
}
