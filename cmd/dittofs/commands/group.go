package commands

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
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

func (c *GroupCommand) openStore() (store.Store, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, err
	}
	return store.New(&cfg.Database)
}

func (c *GroupCommand) runAdd(args []string) error {
	fs := flag.NewFlagSet("group add", flag.ExitOnError)
	gid := fs.Uint("gid", 0, "Group ID (auto-generated if not specified)")
	description := fs.String("description", "", "Group description")
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("group name required\nUsage: dittofs group add <name> [--gid N] [--description TEXT]")
	}

	groupName := fs.Arg(0)

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	// Check if group already exists
	_, err = s.GetGroup(ctx, groupName)
	if err == nil {
		return fmt.Errorf("group %q already exists", groupName)
	}
	if err != models.ErrGroupNotFound {
		return fmt.Errorf("failed to check group: %w", err)
	}

	// Generate GID if not specified
	groupGID := uint32(*gid)
	if groupGID == 0 {
		groupGID, err = c.findNextGID(ctx, s)
		if err != nil {
			return fmt.Errorf("failed to find next GID: %w", err)
		}
	}

	// Create group
	group := &models.Group{
		ID:          uuid.New().String(),
		Name:        groupName,
		GID:         &groupGID,
		Description: *description,
		CreatedAt:   time.Now(),
	}

	if _, err := s.CreateGroup(ctx, group); err != nil {
		return fmt.Errorf("failed to create group: %w", err)
	}

	fmt.Printf("Group %q created (GID: %d)\n", groupName, groupGID)
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

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	// Check if group has members
	if !*force {
		members, err := s.GetGroupMembers(ctx, groupName)
		if err != nil && err != models.ErrGroupNotFound {
			return fmt.Errorf("failed to check group members: %w", err)
		}
		if len(members) > 0 {
			var memberNames []string
			for _, m := range members {
				memberNames = append(memberNames, m.Username)
			}
			return fmt.Errorf("group %q has members (%s). Use --force to delete anyway", groupName, strings.Join(memberNames, ", "))
		}
	}

	if err := s.DeleteGroup(ctx, groupName); err != nil {
		if err == models.ErrGroupNotFound {
			return fmt.Errorf("group %q not found", groupName)
		}
		return fmt.Errorf("failed to delete group: %w", err)
	}

	fmt.Printf("Group %q deleted\n", groupName)
	return nil
}

func (c *GroupCommand) runList(args []string) error {
	fs := flag.NewFlagSet("group list", flag.ExitOnError)
	if err := c.parseFlags(fs, args); err != nil {
		return err
	}

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	groups, err := s.ListGroups(ctx)
	if err != nil {
		return fmt.Errorf("failed to list groups: %w", err)
	}

	if len(groups) == 0 {
		fmt.Println("No groups configured")
		return nil
	}

	fmt.Printf("%-20s %-8s %-8s %s\n", "NAME", "GID", "MEMBERS", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 80))
	for _, g := range groups {
		gid := "-"
		if g.GID != nil {
			gid = fmt.Sprintf("%d", *g.GID)
		}

		// Get member count
		members, _ := s.GetGroupMembers(ctx, g.Name)
		memberCount := len(members)

		description := g.Description
		if description == "" {
			description = "-"
		}

		fmt.Printf("%-20s %-8s %-8d %s\n", g.Name, gid, memberCount, description)
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

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	members, err := s.GetGroupMembers(ctx, groupName)
	if err != nil {
		if err == models.ErrGroupNotFound {
			return fmt.Errorf("group %q not found", groupName)
		}
		return fmt.Errorf("failed to get group members: %w", err)
	}

	if len(members) == 0 {
		fmt.Printf("Group %q has no members\n", groupName)
		return nil
	}

	fmt.Printf("Members of group %q:\n", groupName)
	for _, m := range members {
		fmt.Printf("  - %s\n", m.Username)
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
	perm := models.ParseSharePermission(permission)
	if !perm.IsValid() {
		return fmt.Errorf("invalid permission %q (valid: none, read, read-write, admin)", permission)
	}

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	// Verify group exists
	_, err = s.GetGroup(ctx, groupName)
	if err != nil {
		if err == models.ErrGroupNotFound {
			return fmt.Errorf("group %q not found", groupName)
		}
		return fmt.Errorf("failed to get group: %w", err)
	}

	// Set permission
	groupPerm := &models.GroupSharePermission{
		GroupID:    groupName,
		ShareID:    shareName,
		Permission: permission,
	}

	if err := s.SetGroupSharePermission(ctx, groupPerm); err != nil {
		return fmt.Errorf("failed to set permission: %w", err)
	}

	fmt.Printf("Granted %q permission on %q to group %q\n", permission, shareName, groupName)
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

	s, err := c.openStore()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}

	ctx := context.Background()

	if err := s.DeleteGroupSharePermission(ctx, groupName, shareName); err != nil {
		return fmt.Errorf("failed to revoke permission: %w", err)
	}

	fmt.Printf("Revoked permission on %q from group %q\n", shareName, groupName)
	return nil
}

func (c *GroupCommand) findNextGID(ctx context.Context, s store.Store) (uint32, error) {
	groups, err := s.ListGroups(ctx)
	if err != nil {
		return 0, err
	}

	maxGID := uint32(99) // Start from 100
	for _, g := range groups {
		if g.GID != nil && *g.GID > maxGID {
			maxGID = *g.GID
		}
	}
	return maxGID + 1, nil
}
