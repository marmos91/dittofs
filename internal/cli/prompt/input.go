package prompt

import (
	"github.com/manifoldco/promptui"
)

// Input prompts for text input.
func Input(label string, defaultValue string) (string, error) {
	prompt := promptui.Prompt{
		Label:   label,
		Default: defaultValue,
	}

	return prompt.Run()
}

// InputRequired prompts for required text input.
func InputRequired(label string) (string, error) {
	prompt := promptui.Prompt{
		Label: label,
		Validate: func(input string) error {
			if input == "" {
				return promptui.ErrAbort
			}
			return nil
		},
	}

	return prompt.Run()
}

// InputWithValidation prompts for text input with custom validation.
func InputWithValidation(label string, validate func(string) error) (string, error) {
	prompt := promptui.Prompt{
		Label:    label,
		Validate: validate,
	}

	return prompt.Run()
}

// InputOptional prompts for optional text input.
// Returns empty string if user just presses Enter.
func InputOptional(label string) (string, error) {
	prompt := promptui.Prompt{
		Label: label + " (optional)",
	}

	return prompt.Run()
}
