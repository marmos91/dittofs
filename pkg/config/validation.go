package config

import (
	"fmt"

	"github.com/go-playground/validator/v10"
)

// validate is the singleton validator instance
var validate *validator.Validate

func init() {
	validate = validator.New()
}

// Validate validates the configuration using struct tags and custom rules.
//
// This function uses go-playground/validator for declarative validation
// via struct tags, with additional custom validation for complex rules
// that cannot be expressed in tags.
//
// Note: Log level normalization is handled in ApplyDefaults, not here.
// Validation accepts both uppercase and lowercase log levels.
//
// Returns an error describing validation failures.
func Validate(cfg *Config) error {
	// Run struct tag validation
	if err := validate.Struct(cfg); err != nil {
		return formatValidationError(err)
	}

	// Custom validation rules that can't be expressed in tags
	if err := validateCustomRules(cfg); err != nil {
		return err
	}

	return nil
}

// validateCustomRules performs custom validation beyond struct tags.
func validateCustomRules(cfg *Config) error {
	// Validate at least one share exists
	if len(cfg.Shares) == 0 {
		return fmt.Errorf("shares: at least one share must be configured")
	}

	// Validate share names are unique
	names := make(map[string]bool)
	for i, share := range cfg.Shares {
		if names[share.Name] {
			return fmt.Errorf("shares[%d]: duplicate share name %q", i, share.Name)
		}
		names[share.Name] = true
	}

	// Validate at least one adapter is enabled
	if !cfg.Adapters.NFS.Enabled {
		return fmt.Errorf("adapters: at least one adapter must be enabled")
	}

	// Validate dump_restricted logic for each share
	for i, share := range cfg.Shares {
		if share.DumpRestricted && len(share.DumpAllowedClients) == 0 {
			return fmt.Errorf("shares[%d]: dump_restricted is true but dump_allowed_clients is empty", i)
		}
	}

	return nil
}

// formatValidationError converts validator errors into user-friendly messages.
func formatValidationError(err error) error {
	if validationErrs, ok := err.(validator.ValidationErrors); ok {
		// Return the first validation error with context
		if len(validationErrs) > 0 {
			e := validationErrs[0]
			return fmt.Errorf("%s: validation failed on '%s' tag (value: %v)",
				e.Namespace(), e.Tag(), e.Value())
		}
	}
	return err
}
