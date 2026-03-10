package remote

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editType   string
	editConfig string
	// S3 specific
	editBucket    string
	editRegion    string
	editEndpoint  string
	editAccessKey string
	editSecretKey string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a remote block store",
	Long: `Edit an existing remote block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively
  dfsctl store block remote edit s3-store

  # Update config with JSON
  dfsctl store block remote edit s3-store --config '{"bucket":"new-bucket"}'

  # Update S3 settings
  dfsctl store block remote edit s3-store --bucket new-bucket --region us-west-2`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editType, "type", "", "Store type: s3, memory")
	editCmd.Flags().StringVar(&editConfig, "config", "", "Store configuration as JSON")
	editCmd.Flags().StringVar(&editBucket, "bucket", "", "S3 bucket name (for s3)")
	editCmd.Flags().StringVar(&editRegion, "region", "", "AWS region (for s3)")
	editCmd.Flags().StringVar(&editEndpoint, "endpoint", "", "Custom S3 endpoint")
	editCmd.Flags().StringVar(&editAccessKey, "access-key", "", "AWS access key ID (for s3)")
	editCmd.Flags().StringVar(&editSecretKey, "secret-key", "", "AWS secret access key (for s3)")
}

func runEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	current, err := client.GetBlockStore("remote", name)
	if err != nil {
		return fmt.Errorf("failed to get remote block store: %w", err)
	}

	hasFlags := cmd.Flags().Changed("type") || cmd.Flags().Changed("config") ||
		cmd.Flags().Changed("bucket") || cmd.Flags().Changed("region") || cmd.Flags().Changed("endpoint") ||
		cmd.Flags().Changed("access-key") || cmd.Flags().Changed("secret-key")

	if !hasFlags {
		return runEditInteractive(client, name, current)
	}

	req := &apiclient.UpdateStoreRequest{}
	hasUpdate := false

	if editConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(editConfig), &config); err != nil {
			return fmt.Errorf("invalid JSON config: %w", err)
		}
		req.Config = config
		hasUpdate = true
	} else if editBucket != "" || editRegion != "" || editEndpoint != "" || editAccessKey != "" || editSecretKey != "" {
		currentConfig := cmdutil.ParseConfigMap(current.Config)

		if editBucket != "" {
			currentConfig["bucket"] = editBucket
		}
		if editRegion != "" {
			currentConfig["region"] = editRegion
		}
		if editEndpoint != "" {
			currentConfig["endpoint"] = editEndpoint
		}
		if editAccessKey != "" {
			currentConfig["access_key_id"] = editAccessKey
		}
		if editSecretKey != "" {
			currentConfig["secret_access_key"] = editSecretKey
		}
		req.Config = currentConfig
		hasUpdate = true
	}

	if editType != "" {
		req.Type = &editType
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no update fields specified. Use --type, --config, --bucket, --region, --endpoint, --access-key, or --secret-key")
	}

	store, err := client.UpdateBlockStore("remote", name, req)
	if err != nil {
		return fmt.Errorf("failed to update remote block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Remote block store '%s' updated successfully", store.Name))
}

func runEditInteractive(client *apiclient.Client, name string, current *apiclient.BlockStore) error {
	fmt.Printf("Editing remote block store: %s (type: %s)\n", current.Name, current.Type)
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	currentConfig := cmdutil.ParseConfigMap(current.Config)

	req := &apiclient.UpdateStoreRequest{}
	hasUpdate := false

	switch current.Type {
	case "s3":
		bucket := cmdutil.GetConfigString(currentConfig, "bucket", "")
		region := cmdutil.GetConfigString(currentConfig, "region", "us-east-1")
		endpoint := cmdutil.GetConfigString(currentConfig, "endpoint", "")
		accessKey := cmdutil.GetConfigString(currentConfig, "access_key_id", "")
		hasCredentials := accessKey != ""

		newBucket, err := prompt.Input("S3 bucket name", bucket)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		newRegion, err := prompt.Input("AWS region", region)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		newEndpoint, err := prompt.Input("Custom endpoint (empty for AWS)", endpoint)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}

		accessKeyPrompt := "Access key ID"
		if hasCredentials {
			accessKeyPrompt = fmt.Sprintf("Access key ID (current: %s...)", accessKey[:min(8, len(accessKey))])
		}
		newAccessKey, err := prompt.Input(accessKeyPrompt, accessKey)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}

		var newSecretKey string
		if newAccessKey != "" && newAccessKey != accessKey {
			newSecretKey, err = prompt.Password("Secret access key")
			if err != nil {
				return cmdutil.HandleAbort(err)
			}
		} else if newAccessKey != "" {
			newSecretKey = cmdutil.GetConfigString(currentConfig, "secret_access_key", "")
		}

		newConfig := map[string]any{
			"bucket": newBucket,
			"region": newRegion,
		}
		if newEndpoint != "" {
			newConfig["endpoint"] = newEndpoint
		}
		if newAccessKey != "" {
			newConfig["access_key_id"] = newAccessKey
		}
		if newSecretKey != "" {
			newConfig["secret_access_key"] = newSecretKey
		}

		req.Config = newConfig
		hasUpdate = true

	case "memory":
		fmt.Println("Memory stores have no configurable settings.")
		return nil

	default:
		return fmt.Errorf("unknown store type: %s", current.Type)
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	store, err := client.UpdateBlockStore("remote", name, req)
	if err != nil {
		return fmt.Errorf("failed to update remote block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Remote block store '%s' updated successfully", store.Name))
}
