package commands

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"golang.org/x/term"
)

// UserCommand handles user management subcommands
type UserCommand struct {
	configFile string
}

// NewUserCommand creates a new user command handler
func NewUserCommand() *UserCommand {
	return &UserCommand{}
}

// Run executes the user command with the given arguments
func (c *UserCommand) Run(args []string) error {
	if len(args) < 1 {
		return c.printUsage()
	}

	subcommand := args[0]
	subArgs := args[1:]

	switch subcommand {
	case "add":
		return c.runAdd(subArgs)
	case "delete", "remove":
		return c.runDelete(subArgs)
	case "list", "ls":
		return c.runList(subArgs)
	case "passwd", "password":
		return c.runPasswd(subArgs)
	case "grant":
		return c.runGrant(subArgs)
	case "revoke":
		return c.runRevoke(subArgs)
	case "groups":
		return c.runGroups(subArgs)
	case "join":
		return c.runJoin(subArgs)
	case "leave":
		return c.runLeave(subArgs)
	case "help", "--help", "-h":
		return c.printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown user subcommand: %s\n\n", subcommand)
		return c.printUsage()
	}
}

func (c *UserCommand) printUsage() error {
	fmt.Fprint(os.Stderr, `Usage: dittofs user <subcommand> [options]

Subcommands:
  add <username>                         Add a new user (prompts for password)
  delete <username>                      Delete a user
  list                                   List all users
  passwd <username>                      Change user password
  grant <username> <share> <permission>  Grant share permission to user
  revoke <username> <share>              Revoke share permission from user
  groups <username>                      List groups user belongs to
  join <username> <group>                Add user to a group
  leave <username> <group>               Remove user from a group

Options:
  --config string    Path to config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)

Permissions:
  none        No access
  read        Read-only access
  read-write  Read and write access
  admin       Full administrative access

Examples:
  dittofs user add alice
  dittofs user passwd alice
  dittofs user grant alice /export read-write
  dittofs user revoke alice /export
  dittofs user join alice editors
  dittofs user groups alice
  dittofs user list
`)
	return nil
}

func (c *UserCommand) parseFlags(fs *flag.FlagSet, args []string) error {
	fs.StringVar(&c.configFile, "config", "", "Path to config file")
	return fs.Parse(args)
}

func (c *UserCommand) loadConfig() (*config.Config, error) {
	return config.Load(c.configFile)
}

func (c *UserCommand) openStore() (store.Store, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, err
	}
	return store.New(&cfg.Database)
}

func (c *UserCommand) runAdd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ExitOnError)
	uid := fs.Uint("uid", 0, "User ID (auto-generated if not specified)")
	gid := fs.Uint("gid", 0, "Primary group ID (defaults to UID)")
	groups := fs.String("groups", "", "Comma-separated list of groups")
	role := fs.String("role", "user", "User role (user or admin)")
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("username required\nUsage: dittofs user add <username> [--uid N] [--gid N] [--groups g1,g2] [--role user|admin]")
	}

	username := fs.Arg(0)

	// Open store
	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	// Check if user already exists
	_, err = s.GetUser(ctx, username)
	if err == nil {
		return fmt.Errorf("user %q already exists", username)
	}
	if err != models.ErrUserNotFound {
		return fmt.Errorf("failed to check user: %w", err)
	}

	// Prompt for password
	password, err := promptPassword("Password: ")
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}

	confirm, err := promptPassword("Confirm password: ")
	if err != nil {
		return fmt.Errorf("failed to read password confirmation: %w", err)
	}

	if password != confirm {
		return fmt.Errorf("passwords do not match")
	}

	// Validate password
	if err := models.ValidatePassword(password); err != nil {
		return fmt.Errorf("invalid password: %w", err)
	}

	// Hash password (bcrypt for general auth)
	hash, err := models.HashPassword(password)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Compute NT hash for SMB NTLM authentication
	ntHash := models.ComputeNTHash(password)
	ntHashHex := fmt.Sprintf("%x", ntHash)

	// Generate UID if not specified
	userUID := uint32(*uid)
	if userUID == 0 {
		userUID, err = c.findNextUID(ctx, s)
		if err != nil {
			return fmt.Errorf("failed to find next UID: %w", err)
		}
	}

	// Default GID to UID if not specified
	userGID := uint32(*gid)
	if userGID == 0 {
		userGID = userUID
	}

	// Create user
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     username,
		PasswordHash: hash,
		NTHash:       ntHashHex,
		Enabled:      true,
		Role:         *role,
		UID:          &userUID,
		GID:          &userGID,
		CreatedAt:    time.Now(),
	}

	if _, err := s.CreateUser(ctx, user); err != nil {
		return fmt.Errorf("failed to create user: %w", err)
	}

	// Add to groups if specified
	if *groups != "" {
		groupList := strings.Split(*groups, ",")
		for _, groupName := range groupList {
			groupName = strings.TrimSpace(groupName)
			if groupName == "" {
				continue
			}
			if err := s.AddUserToGroup(ctx, username, groupName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to add user to group %q: %v\n", groupName, err)
			}
		}
	}

	fmt.Printf("User %q created (UID: %d, GID: %d)\n", username, userUID, userGID)
	return nil
}

func (c *UserCommand) runDelete(args []string) error {
	fs := flag.NewFlagSet("user delete", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("username required\nUsage: dittofs user delete <username>")
	}

	username := fs.Arg(0)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	if err := s.DeleteUser(ctx, username); err != nil {
		if err == models.ErrUserNotFound {
			return fmt.Errorf("user %q not found", username)
		}
		return fmt.Errorf("failed to delete user: %w", err)
	}

	fmt.Printf("User %q deleted\n", username)
	return nil
}

func (c *UserCommand) runList(args []string) error {
	fs := flag.NewFlagSet("user list", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	users, err := s.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list users: %w", err)
	}

	if len(users) == 0 {
		fmt.Println("No users configured")
		return nil
	}

	fmt.Printf("%-20s %-8s %-8s %-8s %-8s %s\n", "USERNAME", "UID", "GID", "ROLE", "ENABLED", "GROUPS")
	fmt.Println(strings.Repeat("-", 80))
	for _, u := range users {
		enabled := "yes"
		if !u.Enabled {
			enabled = "no"
		}
		uid := "-"
		if u.UID != nil {
			uid = fmt.Sprintf("%d", *u.UID)
		}
		gid := "-"
		if u.GID != nil {
			gid = fmt.Sprintf("%d", *u.GID)
		}

		// Get user's groups
		var groupNames []string
		for _, g := range u.Groups {
			groupNames = append(groupNames, g.Name)
		}
		groups := strings.Join(groupNames, ",")
		if groups == "" {
			groups = "-"
		}

		fmt.Printf("%-20s %-8s %-8s %-8s %-8s %s\n", u.Username, uid, gid, u.Role, enabled, groups)
	}

	return nil
}

func (c *UserCommand) runPasswd(args []string) error {
	fs := flag.NewFlagSet("user passwd", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("username required\nUsage: dittofs user passwd <username>")
	}

	username := fs.Arg(0)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	// Get user to verify exists
	user, err := s.GetUser(ctx, username)
	if err != nil {
		if err == models.ErrUserNotFound {
			return fmt.Errorf("user %q not found", username)
		}
		return fmt.Errorf("failed to get user: %w", err)
	}

	// Prompt for new password
	password, err := promptPassword("New password: ")
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}

	confirm, err := promptPassword("Confirm password: ")
	if err != nil {
		return fmt.Errorf("failed to read password confirmation: %w", err)
	}

	if password != confirm {
		return fmt.Errorf("passwords do not match")
	}

	if err := models.ValidatePassword(password); err != nil {
		return fmt.Errorf("invalid password: %w", err)
	}

	hash, err := models.HashPassword(password)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Compute NT hash for SMB NTLM authentication
	ntHash := models.ComputeNTHash(password)
	ntHashHex := fmt.Sprintf("%x", ntHash)

	user.PasswordHash = hash
	user.NTHash = ntHashHex
	user.MustChangePassword = false

	if err := s.UpdateUser(ctx, user); err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}

	fmt.Printf("Password changed for user %q\n", username)
	return nil
}

func (c *UserCommand) runGrant(args []string) error {
	fs := flag.NewFlagSet("user grant", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 3 {
		return fmt.Errorf("username, share, and permission required\nUsage: dittofs user grant <username> <share> <permission>")
	}

	username := fs.Arg(0)
	shareName := fs.Arg(1)
	permission := fs.Arg(2)

	// Validate permission
	perm := models.ParseSharePermission(permission)
	if !perm.IsValid() {
		return fmt.Errorf("invalid permission %q (valid: none, read, read-write, admin)", permission)
	}

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	// Get user to obtain ID
	user, err := s.GetUser(ctx, username)
	if err != nil {
		if err == models.ErrUserNotFound {
			return fmt.Errorf("user %q not found", username)
		}
		return fmt.Errorf("failed to get user: %w", err)
	}

	// Get share to obtain ID
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		if err == models.ErrShareNotFound {
			return fmt.Errorf("share %q not found", shareName)
		}
		return fmt.Errorf("failed to get share: %w", err)
	}

	// Set permission with correct IDs
	userPerm := &models.UserSharePermission{
		UserID:     user.ID,
		ShareID:    share.ID,
		ShareName:  share.Name,
		Permission: permission,
	}

	if err := s.SetUserSharePermission(ctx, userPerm); err != nil {
		return fmt.Errorf("failed to set permission: %w", err)
	}

	fmt.Printf("Granted %q permission on %q to user %q\n", permission, shareName, username)
	return nil
}

func (c *UserCommand) runRevoke(args []string) error {
	fs := flag.NewFlagSet("user revoke", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 2 {
		return fmt.Errorf("username and share required\nUsage: dittofs user revoke <username> <share>")
	}

	username := fs.Arg(0)
	shareName := fs.Arg(1)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	if err := s.DeleteUserSharePermission(ctx, username, shareName); err != nil {
		return fmt.Errorf("failed to revoke permission: %w", err)
	}

	fmt.Printf("Revoked permission on %q from user %q\n", shareName, username)
	return nil
}

func (c *UserCommand) runGroups(args []string) error {
	fs := flag.NewFlagSet("user groups", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("username required\nUsage: dittofs user groups <username>")
	}

	username := fs.Arg(0)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	user, err := s.GetUser(ctx, username)
	if err != nil {
		if err == models.ErrUserNotFound {
			return fmt.Errorf("user %q not found", username)
		}
		return fmt.Errorf("failed to get user: %w", err)
	}

	if len(user.Groups) == 0 {
		fmt.Printf("User %q is not a member of any groups\n", username)
		return nil
	}

	fmt.Printf("Groups for user %q:\n", username)
	for _, g := range user.Groups {
		fmt.Printf("  - %s\n", g.Name)
	}

	return nil
}

func (c *UserCommand) runJoin(args []string) error {
	fs := flag.NewFlagSet("user join", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 2 {
		return fmt.Errorf("username and group required\nUsage: dittofs user join <username> <group>")
	}

	username := fs.Arg(0)
	groupName := fs.Arg(1)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	if err := s.AddUserToGroup(ctx, username, groupName); err != nil {
		if err == models.ErrUserNotFound {
			return fmt.Errorf("user %q not found", username)
		}
		if err == models.ErrGroupNotFound {
			return fmt.Errorf("group %q not found", groupName)
		}
		return fmt.Errorf("failed to add user to group: %w", err)
	}

	fmt.Printf("Added user %q to group %q\n", username, groupName)
	return nil
}

func (c *UserCommand) runLeave(args []string) error {
	fs := flag.NewFlagSet("user leave", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 2 {
		return fmt.Errorf("username and group required\nUsage: dittofs user leave <username> <group>")
	}

	username := fs.Arg(0)
	groupName := fs.Arg(1)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	if err := s.RemoveUserFromGroup(ctx, username, groupName); err != nil {
		return fmt.Errorf("failed to remove user from group: %w", err)
	}

	fmt.Printf("Removed user %q from group %q\n", username, groupName)
	return nil
}

func (c *UserCommand) findNextUID(ctx context.Context, s store.Store) (uint32, error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return 0, err
	}

	maxUID := uint32(999) // Start from 1000
	for _, u := range users {
		if u.UID != nil && *u.UID > maxUID {
			maxUID = *u.UID
		}
	}
	return maxUID + 1, nil
}

// promptPassword prompts for a password without echoing
func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)

	// Check if stdin is a terminal
	if term.IsTerminal(int(syscall.Stdin)) {
		password, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Println() // Print newline after password input
		if err != nil {
			return "", err
		}
		return string(password), nil
	}

	// Fall back to reading from stdin (for piped input)
	reader := bufio.NewReader(os.Stdin)
	password, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(password), nil
}
