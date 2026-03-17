package share

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editLocal             string
	editRemote            string
	editReadOnly          string
	editEncryptData       string
	editDefaultPermission string
	editDescription       string
	editRetention         string
	editRetentionTTL      string
	editLocalStoreSize    string
	editReadBufferSize    string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a share",
	Long: `Edit an existing share on the DittoFS server.

When run without flags, opens an interactive editor to modify share properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit share interactively
  dfsctl share edit /archive

  # Update local block store reference
  dfsctl share edit /archive --local new-fs-cache

  # Update remote block store reference
  dfsctl share edit /archive --remote new-s3-store

  # Make share read-only
  dfsctl share edit /archive --read-only true

  # Make share writable
  dfsctl share edit /archive --read-only false

  # Set default permission to allow all users read-write access
  dfsctl share edit /archive --default-permission read-write

  # Update description
  dfsctl share edit /archive --description "New description"

  # Change retention policy to pin (blocks never evicted)
  dfsctl share edit /archive --retention pin

  # Change retention policy to TTL with 72-hour window
  dfsctl share edit /archive --retention ttl --retention-ttl 72h

  # Override per-share disk cache size
  dfsctl share edit /archive --local-store-size 10GiB

  # Override per-share read buffer size
  dfsctl share edit /archive --read-buffer-size 2GiB`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editLocal, "local", "", "Local block store name")
	editCmd.Flags().StringVar(&editRemote, "remote", "", "Remote block store name")
	editCmd.Flags().StringVar(&editReadOnly, "read-only", "", "Set read-only (true|false)")
	editCmd.Flags().StringVar(&editEncryptData, "encrypt-data", "", "Require SMB3 encryption (true|false)")
	editCmd.Flags().StringVar(&editDefaultPermission, "default-permission", "", "Default permission (none|read|read-write|admin)")
	editCmd.Flags().StringVar(&editDescription, "description", "", "Share description")
	editCmd.Flags().StringVar(&editRetention, "retention", "", "Retention policy (pin|ttl|lru)")
	editCmd.Flags().StringVar(&editRetentionTTL, "retention-ttl", "", "Retention TTL duration (e.g., 72h)")
	editCmd.Flags().StringVar(&editLocalStoreSize, "local-store-size", "", "Per-share disk cache size override (e.g., 10GiB, 500MiB)")
	editCmd.Flags().StringVar(&editReadBufferSize, "read-buffer-size", "", "Per-share read buffer size override (e.g., 2GiB, 256MiB)")
}

func runEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Check if any flags were provided
	hasFlags := cmd.Flags().Changed("local") || cmd.Flags().Changed("remote") ||
		cmd.Flags().Changed("read-only") || cmd.Flags().Changed("encrypt-data") ||
		cmd.Flags().Changed("default-permission") ||
		cmd.Flags().Changed("description") || cmd.Flags().Changed("retention") ||
		cmd.Flags().Changed("retention-ttl") || cmd.Flags().Changed("local-store-size") ||
		cmd.Flags().Changed("read-buffer-size")

	// If no flags provided, run interactive mode
	if !hasFlags {
		return runEditInteractive(client, name)
	}

	// Build update request with only specified fields
	req := &apiclient.UpdateShareRequest{}
	hasUpdate := false

	if editLocal != "" {
		req.LocalBlockStoreID = &editLocal
		hasUpdate = true
	}

	if editRemote != "" {
		req.RemoteBlockStoreID = &editRemote
		hasUpdate = true
	}

	if editReadOnly != "" {
		readOnly := strings.ToLower(editReadOnly) == "true"
		req.ReadOnly = &readOnly
		hasUpdate = true
	}

	if editEncryptData != "" {
		encryptData := strings.ToLower(editEncryptData) == "true"
		req.EncryptData = &encryptData
		hasUpdate = true
	}

	if editDefaultPermission != "" {
		req.DefaultPermission = &editDefaultPermission
		hasUpdate = true
	}

	if editDescription != "" {
		req.Description = &editDescription
		hasUpdate = true
	}

	if editRetention != "" {
		req.RetentionPolicy = &editRetention
		hasUpdate = true
	}

	if editRetentionTTL != "" {
		req.RetentionTTL = &editRetentionTTL
		hasUpdate = true
	}

	if editLocalStoreSize != "" {
		req.LocalStoreSize = &editLocalStoreSize
		hasUpdate = true
	}

	if editReadBufferSize != "" {
		req.ReadBufferSize = &editReadBufferSize
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no fields specified. Use --local, --remote, --read-only, --default-permission, --description, --retention, --retention-ttl, --local-store-size, or --read-buffer-size")
	}

	share, err := client.UpdateShare(name, req)
	if err != nil {
		return fmt.Errorf("failed to update share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' updated successfully", share.Name))
}

func runEditInteractive(client *apiclient.Client, name string) error {
	// Fetch current share
	current, err := client.GetShare(name)
	if err != nil {
		return fmt.Errorf("failed to get share: %w", err)
	}

	fmt.Printf("Editing share: %s\n", current.Name)
	fmt.Println("Press Enter to keep current value, or enter a new value.")
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	req := &apiclient.UpdateShareRequest{}
	hasUpdate := false

	// Description
	newDescription, err := prompt.Input("Description", current.Description)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newDescription != current.Description {
		req.Description = &newDescription
		hasUpdate = true
	}

	// Read-only
	readOnlyOptions := []prompt.SelectOption{
		{Label: "writable", Value: "false", Description: "Allow write operations"},
		{Label: "read-only", Value: "true", Description: "Only allow read operations"},
	}
	currentReadOnlyStatus := "writable"
	if current.ReadOnly {
		currentReadOnlyStatus = "read-only"
	}
	fmt.Printf("Currently: %s\n", currentReadOnlyStatus)
	newReadOnlyStr, err := prompt.Select("Access mode", readOnlyOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	newReadOnly := newReadOnlyStr == "true"
	if newReadOnly != current.ReadOnly {
		req.ReadOnly = &newReadOnly
		hasUpdate = true
	}

	// Default permission
	permOptions := []prompt.SelectOption{
		{Label: "none", Value: "none", Description: "No default access"},
		{Label: "read", Value: "read", Description: "Read-only access by default"},
		{Label: "read-write", Value: "read-write", Description: "Read-write access by default"},
		{Label: "admin", Value: "admin", Description: "Admin access by default"},
	}
	currentPerm := current.DefaultPermission
	if currentPerm == "" {
		currentPerm = "none"
	}
	fmt.Printf("Current default permission: %s\n", currentPerm)
	newPerm, err := prompt.Select("Default permission", permOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newPerm != currentPerm {
		req.DefaultPermission = &newPerm
		hasUpdate = true
	}

	// Retention policy
	retOptions := []string{"lru", "ttl", "pin"}
	currentRet := current.RetentionPolicy
	if currentRet == "" {
		currentRet = "lru"
	}
	fmt.Printf("Current retention: %s\n", currentRet)
	newRet, err := prompt.SelectString("Retention policy", retOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newRet != currentRet {
		req.RetentionPolicy = &newRet
		hasUpdate = true
	}

	// Retention TTL (only prompt if TTL policy selected)
	if newRet == "ttl" {
		currentTTL := current.RetentionTTL
		if currentTTL == "" {
			currentTTL = "24h"
		}
		newTTLStr, err := prompt.Input("Retention TTL", currentTTL)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		if newTTLStr != current.RetentionTTL {
			req.RetentionTTL = &newTTLStr
			hasUpdate = true
		}
	}

	// Local store size override
	currentLocalStoreSize := current.LocalStoreSize
	if currentLocalStoreSize == "" {
		currentLocalStoreSize = "0 (system default)"
	}
	fmt.Printf("Current local store size: %s\n", currentLocalStoreSize)
	newLocalStoreSize, err := prompt.Input("Local store size (0 for system default)", current.LocalStoreSize)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newLocalStoreSize != current.LocalStoreSize {
		req.LocalStoreSize = &newLocalStoreSize
		hasUpdate = true
	}

	// Read buffer size override
	currentReadBufferSize := current.ReadBufferSize
	if currentReadBufferSize == "" {
		currentReadBufferSize = "0 (system default)"
	}
	fmt.Printf("Current read buffer size: %s\n", currentReadBufferSize)
	newReadBufferSize, err := prompt.Input("Read buffer size (0 for system default)", current.ReadBufferSize)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newReadBufferSize != current.ReadBufferSize {
		req.ReadBufferSize = &newReadBufferSize
		hasUpdate = true
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	share, err := client.UpdateShare(name, req)
	if err != nil {
		return fmt.Errorf("failed to update share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' updated successfully", share.Name))
}
