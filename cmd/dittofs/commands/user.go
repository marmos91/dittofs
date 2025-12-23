package commands

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/identity"
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

func (c *UserCommand) saveConfig(cfg *config.Config) error {
	path := c.configFile
	if path == "" {
		path = config.GetDefaultConfigPath()
	}
	return config.SaveConfig(cfg, path)
}

func (c *UserCommand) runAdd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ExitOnError)
	uid := fs.Uint("uid", 0, "User ID (auto-generated if not specified)")
	gid := fs.Uint("gid", 0, "Primary group ID (defaults to UID)")
	groups := fs.String("groups", "", "Comma-separated list of groups")
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("username required\nUsage: dittofs user add <username> [--uid N] [--gid N] [--groups g1,g2]")
	}

	username := fs.Arg(0)

	// Load config
	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if user already exists
	for _, u := range cfg.Users {
		if u.Username == username {
			return fmt.Errorf("user %q already exists", username)
		}
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
	if err := identity.ValidatePassword(password); err != nil {
		return fmt.Errorf("invalid password: %w", err)
	}

	// Hash password (bcrypt for general auth)
	hash, err := identity.HashPassword(password)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Compute NT hash for SMB NTLM authentication
	ntHash := identity.ComputeNTHash(password)
	ntHashHex := fmt.Sprintf("%x", ntHash)

	// Generate UID if not specified
	userUID := uint32(*uid)
	if userUID == 0 {
		userUID = c.findNextUID(cfg)
	}

	// Default GID to UID if not specified
	userGID := uint32(*gid)
	if userGID == 0 {
		userGID = userUID
	}

	// Parse groups
	var userGroups []string
	if *groups != "" {
		userGroups = strings.Split(*groups, ",")
		for i := range userGroups {
			userGroups[i] = strings.TrimSpace(userGroups[i])
		}
	}

	// Create user config
	newUser := config.UserConfig{
		Username:     username,
		PasswordHash: hash,
		NTHash:       ntHashHex,
		Enabled:      true,
		UID:          userUID,
		GID:          userGID,
		Groups:       userGroups,
	}

	cfg.Users = append(cfg.Users, newUser)

	// Save config
	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ User %q created (UID: %d, GID: %d)\n", username, userUID, userGID)
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

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find and remove user
	found := false
	newUsers := make([]config.UserConfig, 0, len(cfg.Users))
	for _, u := range cfg.Users {
		if u.Username == username {
			found = true
			continue
		}
		newUsers = append(newUsers, u)
	}

	if !found {
		return fmt.Errorf("user %q not found", username)
	}

	cfg.Users = newUsers

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ User %q deleted\n", username)
	return nil
}

func (c *UserCommand) runList(args []string) error {
	fs := flag.NewFlagSet("user list", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if len(cfg.Users) == 0 {
		fmt.Println("No users configured")
		return nil
	}

	fmt.Printf("%-20s %-8s %-8s %-8s %s\n", "USERNAME", "UID", "GID", "ENABLED", "GROUPS")
	fmt.Println(strings.Repeat("-", 70))
	for _, u := range cfg.Users {
		enabled := "yes"
		if !u.Enabled {
			enabled = "no"
		}
		groups := strings.Join(u.Groups, ",")
		if groups == "" {
			groups = "-"
		}
		fmt.Printf("%-20s %-8d %-8d %-8s %s\n", u.Username, u.UID, u.GID, enabled, groups)
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

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find user
	userIdx := -1
	for i, u := range cfg.Users {
		if u.Username == username {
			userIdx = i
			break
		}
	}

	if userIdx == -1 {
		return fmt.Errorf("user %q not found", username)
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

	if err := identity.ValidatePassword(password); err != nil {
		return fmt.Errorf("invalid password: %w", err)
	}

	hash, err := identity.HashPassword(password)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Compute NT hash for SMB NTLM authentication
	ntHash := identity.ComputeNTHash(password)
	ntHashHex := fmt.Sprintf("%x", ntHash)

	cfg.Users[userIdx].PasswordHash = hash
	cfg.Users[userIdx].NTHash = ntHashHex

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Password changed for user %q\n", username)
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
	perm := identity.ParseSharePermission(permission)
	if !perm.IsValid() {
		return fmt.Errorf("invalid permission %q (valid: none, read, read-write, admin)", permission)
	}

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find user
	userIdx := -1
	for i, u := range cfg.Users {
		if u.Username == username {
			userIdx = i
			break
		}
	}

	if userIdx == -1 {
		return fmt.Errorf("user %q not found", username)
	}

	// Initialize share permissions map if nil
	if cfg.Users[userIdx].SharePermissions == nil {
		cfg.Users[userIdx].SharePermissions = make(map[string]string)
	}

	cfg.Users[userIdx].SharePermissions[shareName] = permission

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Granted %q permission on %q to user %q\n", permission, shareName, username)
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

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find user
	userIdx := -1
	for i, u := range cfg.Users {
		if u.Username == username {
			userIdx = i
			break
		}
	}

	if userIdx == -1 {
		return fmt.Errorf("user %q not found", username)
	}

	if cfg.Users[userIdx].SharePermissions == nil {
		return fmt.Errorf("user %q has no explicit permissions on %q", username, shareName)
	}

	if _, ok := cfg.Users[userIdx].SharePermissions[shareName]; !ok {
		return fmt.Errorf("user %q has no explicit permission on %q", username, shareName)
	}

	delete(cfg.Users[userIdx].SharePermissions, shareName)

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Revoked permission on %q from user %q\n", shareName, username)
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

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find user
	var user *config.UserConfig
	for i := range cfg.Users {
		if cfg.Users[i].Username == username {
			user = &cfg.Users[i]
			break
		}
	}

	if user == nil {
		return fmt.Errorf("user %q not found", username)
	}

	if len(user.Groups) == 0 {
		fmt.Printf("User %q is not a member of any groups\n", username)
		return nil
	}

	fmt.Printf("Groups for user %q:\n", username)
	for _, g := range user.Groups {
		fmt.Printf("  - %s\n", g)
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

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find user
	userIdx := -1
	for i, u := range cfg.Users {
		if u.Username == username {
			userIdx = i
			break
		}
	}

	if userIdx == -1 {
		return fmt.Errorf("user %q not found", username)
	}

	// Check if group exists
	groupExists := false
	for _, g := range cfg.Groups {
		if g.Name == groupName {
			groupExists = true
			break
		}
	}

	if !groupExists {
		return fmt.Errorf("group %q not found", groupName)
	}

	// Check if already a member
	for _, g := range cfg.Users[userIdx].Groups {
		if g == groupName {
			return fmt.Errorf("user %q is already a member of group %q", username, groupName)
		}
	}

	cfg.Users[userIdx].Groups = append(cfg.Users[userIdx].Groups, groupName)

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Added user %q to group %q\n", username, groupName)
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

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find user
	userIdx := -1
	for i, u := range cfg.Users {
		if u.Username == username {
			userIdx = i
			break
		}
	}

	if userIdx == -1 {
		return fmt.Errorf("user %q not found", username)
	}

	// Find and remove group
	found := false
	newGroups := make([]string, 0, len(cfg.Users[userIdx].Groups))
	for _, g := range cfg.Users[userIdx].Groups {
		if g == groupName {
			found = true
			continue
		}
		newGroups = append(newGroups, g)
	}

	if !found {
		return fmt.Errorf("user %q is not a member of group %q", username, groupName)
	}

	cfg.Users[userIdx].Groups = newGroups

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Removed user %q from group %q\n", username, groupName)
	return nil
}

func (c *UserCommand) findNextUID(cfg *config.Config) uint32 {
	maxUID := uint32(999) // Start from 1000
	for _, u := range cfg.Users {
		if u.UID > maxUID {
			maxUID = u.UID
		}
	}
	return maxUID + 1
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
