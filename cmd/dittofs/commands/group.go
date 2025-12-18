package commands

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/identity"
)

// GroupCommand handles group management subcommands
type GroupCommand struct {
	configFile string
}

// NewGroupCommand creates a new group command handler
func NewGroupCommand() *GroupCommand {
	return &GroupCommand{}
}

// Run executes the group command with the given arguments
func (c *GroupCommand) Run(args []string) error {
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
	case "members":
		return c.runMembers(subArgs)
	case "grant":
		return c.runGrant(subArgs)
	case "revoke":
		return c.runRevoke(subArgs)
	case "help", "--help", "-h":
		return c.printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown group subcommand: %s\n\n", subcommand)
		return c.printUsage()
	}
}

func (c *GroupCommand) printUsage() error {
	fmt.Fprint(os.Stderr, `Usage: dittofs group <subcommand> [options]

Subcommands:
  add <name>                            Add a new group
  delete <name>                         Delete a group
  list                                  List all groups
  members <name>                        List members of a group
  grant <group> <share> <permission>    Grant share permission to group
  revoke <group> <share>                Revoke share permission from group

Options:
  --config string    Path to config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)

Permissions:
  none        No access
  read        Read-only access
  read-write  Read and write access
  admin       Full administrative access

Examples:
  dittofs group add editors
  dittofs group grant editors /export read-write
  dittofs group revoke editors /export
  dittofs group members editors
  dittofs group list
`)
	return nil
}

func (c *GroupCommand) parseFlags(fs *flag.FlagSet, args []string) error {
	fs.StringVar(&c.configFile, "config", "", "Path to config file")
	return fs.Parse(args)
}

func (c *GroupCommand) loadConfig() (*config.Config, error) {
	return config.Load(c.configFile)
}

func (c *GroupCommand) saveConfig(cfg *config.Config) error {
	path := c.configFile
	if path == "" {
		path = config.GetDefaultConfigPath()
	}
	return config.SaveConfig(cfg, path)
}

func (c *GroupCommand) runAdd(args []string) error {
	fs := flag.NewFlagSet("group add", flag.ExitOnError)
	gid := fs.Uint("gid", 0, "Group ID (auto-generated if not specified)")
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("group name required\nUsage: dittofs group add <name> [--gid N]")
	}

	groupName := fs.Arg(0)

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if group already exists
	for _, g := range cfg.Groups {
		if g.Name == groupName {
			return fmt.Errorf("group %q already exists", groupName)
		}
	}

	// Generate GID if not specified
	groupGID := uint32(*gid)
	if groupGID == 0 {
		groupGID = c.findNextGID(cfg)
	}

	// Create group config
	newGroup := config.GroupConfig{
		Name: groupName,
		GID:  groupGID,
	}

	cfg.Groups = append(cfg.Groups, newGroup)

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Group %q created (GID: %d)\n", groupName, groupGID)
	return nil
}

func (c *GroupCommand) runDelete(args []string) error {
	fs := flag.NewFlagSet("group delete", flag.ExitOnError)
	force := fs.Bool("force", false, "Force delete even if users are members")
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("group name required\nUsage: dittofs group delete <name> [--force]")
	}

	groupName := fs.Arg(0)

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if any users are members
	if !*force {
		members := c.findGroupMembers(cfg, groupName)
		if len(members) > 0 {
			return fmt.Errorf("group %q has members (%s). Use --force to delete anyway", groupName, strings.Join(members, ", "))
		}
	}

	// Find and remove group
	found := false
	newGroups := make([]config.GroupConfig, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		if g.Name == groupName {
			found = true
			continue
		}
		newGroups = append(newGroups, g)
	}

	if !found {
		return fmt.Errorf("group %q not found", groupName)
	}

	cfg.Groups = newGroups

	// Remove group from all users if force delete
	if *force {
		for i := range cfg.Users {
			newUserGroups := make([]string, 0)
			for _, g := range cfg.Users[i].Groups {
				if g != groupName {
					newUserGroups = append(newUserGroups, g)
				}
			}
			cfg.Users[i].Groups = newUserGroups
		}
	}

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Group %q deleted\n", groupName)
	return nil
}

func (c *GroupCommand) runList(args []string) error {
	fs := flag.NewFlagSet("group list", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if len(cfg.Groups) == 0 {
		fmt.Println("No groups configured")
		return nil
	}

	fmt.Printf("%-20s %-8s %-8s %s\n", "NAME", "GID", "MEMBERS", "SHARE PERMISSIONS")
	fmt.Println(strings.Repeat("-", 80))
	for _, g := range cfg.Groups {
		members := c.findGroupMembers(cfg, g.Name)
		memberCount := len(members)

		// Format share permissions
		var perms []string
		for share, perm := range g.SharePermissions {
			perms = append(perms, fmt.Sprintf("%s:%s", share, perm))
		}
		permStr := strings.Join(perms, ", ")
		if permStr == "" {
			permStr = "-"
		}

		fmt.Printf("%-20s %-8d %-8d %s\n", g.Name, g.GID, memberCount, permStr)
	}

	return nil
}

func (c *GroupCommand) runMembers(args []string) error {
	fs := flag.NewFlagSet("group members", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("group name required\nUsage: dittofs group members <name>")
	}

	groupName := fs.Arg(0)

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
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

	members := c.findGroupMembers(cfg, groupName)

	if len(members) == 0 {
		fmt.Printf("Group %q has no members\n", groupName)
		return nil
	}

	fmt.Printf("Members of group %q:\n", groupName)
	for _, m := range members {
		fmt.Printf("  - %s\n", m)
	}

	return nil
}

func (c *GroupCommand) runGrant(args []string) error {
	fs := flag.NewFlagSet("group grant", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 3 {
		return fmt.Errorf("group, share, and permission required\nUsage: dittofs group grant <group> <share> <permission>")
	}

	groupName := fs.Arg(0)
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

	// Find group
	groupIdx := -1
	for i, g := range cfg.Groups {
		if g.Name == groupName {
			groupIdx = i
			break
		}
	}

	if groupIdx == -1 {
		return fmt.Errorf("group %q not found", groupName)
	}

	// Initialize share permissions map if nil
	if cfg.Groups[groupIdx].SharePermissions == nil {
		cfg.Groups[groupIdx].SharePermissions = make(map[string]string)
	}

	cfg.Groups[groupIdx].SharePermissions[shareName] = permission

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Granted %q permission on %q to group %q\n", permission, shareName, groupName)
	return nil
}

func (c *GroupCommand) runRevoke(args []string) error {
	fs := flag.NewFlagSet("group revoke", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 2 {
		return fmt.Errorf("group and share required\nUsage: dittofs group revoke <group> <share>")
	}

	groupName := fs.Arg(0)
	shareName := fs.Arg(1)

	cfg, err := c.loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Find group
	groupIdx := -1
	for i, g := range cfg.Groups {
		if g.Name == groupName {
			groupIdx = i
			break
		}
	}

	if groupIdx == -1 {
		return fmt.Errorf("group %q not found", groupName)
	}

	if cfg.Groups[groupIdx].SharePermissions == nil {
		return fmt.Errorf("group %q has no permissions on %q", groupName, shareName)
	}

	if _, ok := cfg.Groups[groupIdx].SharePermissions[shareName]; !ok {
		return fmt.Errorf("group %q has no permission on %q", groupName, shareName)
	}

	delete(cfg.Groups[groupIdx].SharePermissions, shareName)

	if err := c.saveConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Revoked permission on %q from group %q\n", shareName, groupName)
	return nil
}

func (c *GroupCommand) findNextGID(cfg *config.Config) uint32 {
	maxGID := uint32(99) // Start from 100
	for _, g := range cfg.Groups {
		if g.GID > maxGID {
			maxGID = g.GID
		}
	}
	return maxGID + 1
}

func (c *GroupCommand) findGroupMembers(cfg *config.Config, groupName string) []string {
	var members []string
	for _, u := range cfg.Users {
		for _, g := range u.Groups {
			if g == groupName {
				members = append(members, u.Username)
				break
			}
		}
	}
	return members
}
