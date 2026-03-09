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
// Uses go-playground/validator for declarative validation via struct tags,
// with additional custom rules that cannot be expressed in tags.
// Log level normalization is handled in ApplyDefaults, not here.
func Validate(cfg *Config) error {
	if err := validate.Struct(cfg); err != nil {
		return formatValidationError(err)
	}

	if cfg.Cache.Path == "" {
		return fmt.Errorf("cache: path is required")
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
