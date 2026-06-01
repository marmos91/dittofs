package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var schemaOutput string

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Generate JSON schema for configuration",
	Long: `Generate a JSON schema for the DittoFS configuration file.

The schema can be used for:
  - IDE autocompletion (VS Code, IntelliJ, etc.)
  - Configuration file validation
  - Documentation generation

Examples:
  # Print schema to stdout
  dfs config schema

  # Save schema to file
  dfs config schema --output config.schema.json`,
	RunE: runSchema,
}

func init() {
	schemaCmd.Flags().StringVarP(&schemaOutput, "output", "o", "", "Output file (default: stdout)")
}

func runSchema(cmd *cobra.Command, args []string) error {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
		// The Config structs carry only mapstructure/yaml tags (no json
		// tags). Without this, the reflector falls back to Go field names
		// (PascalCase) and the emitted schema rejects every valid
		// lowercase-keyed config.yaml. Drive field names from the yaml
		// tags so the schema matches the real config namespace.
		FieldNameTag: "yaml",
	}

	schema := reflector.Reflect(&config.Config{})
	schema.Version = "https://json-schema.org/draft/2020-12/schema"
	schema.Title = "DittoFS Configuration"
	schema.Description = "Configuration schema for DittoFS server"

	schemaJSON, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to generate schema: %w", err)
	}

	if schemaOutput != "" {
		if err := os.WriteFile(schemaOutput, schemaJSON, 0644); err != nil {
			return fmt.Errorf("failed to write schema file: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "JSON schema written to %s\n", schemaOutput)
		return nil
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(schemaJSON))
	return nil
}
