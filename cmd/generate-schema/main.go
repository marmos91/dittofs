package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/marmos91/dittofs/pkg/config"
)

func main() {
	// Generate JSON schema from Config struct
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true, // Inline all definitions for simplicity
	}

	schema := reflector.Reflect(&config.Config{})

	// Add schema metadata
	schema.Version = "https://json-schema.org/draft/2020-12/schema"
	schema.Title = "DittoFS Configuration"
	schema.Description = "Configuration schema for DittoFS server (version 1.0.0)"

	// Marshal to pretty JSON
	schemaJSON, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling schema: %v\n", err)
		os.Exit(1)
	}

	// Write to file
	outputFile := "config.schema.json"
	if len(os.Args) > 1 {
		outputFile = os.Args[1]
	}

	if err := os.WriteFile(outputFile, schemaJSON, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing schema file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("JSON schema written to %s\n", outputFile)
}
