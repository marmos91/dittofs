package prompt

import (
	"errors"

	"github.com/manifoldco/promptui"
)

// ErrAborted is returned when the user aborts a prompt (Ctrl+C).
var ErrAborted = errors.New("aborted")

// IsAborted returns true if the error indicates the user aborted (Ctrl+C).
func IsAborted(err error) bool {
	return errors.Is(err, promptui.ErrInterrupt) || errors.Is(err, promptui.ErrAbort) || errors.Is(err, ErrAborted)
}

// wrapError converts promptui interrupt/abort errors to ErrAborted for consistent handling.
func wrapError(err error) error {
	if err == nil {
		return nil
	}
	if IsAborted(err) {
		return ErrAborted
	}
	return err
}

// Input prompts for text input.
func Input(label string, defaultValue string) (string, error) {
	prompt := promptui.Prompt{
		Label:   label,
		Default: defaultValue,
	}

	result, err := prompt.Run()
	return result, wrapError(err)
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

	result, err := prompt.Run()
	return result, wrapError(err)
}

// InputWithValidation prompts for text input with custom validation.
func InputWithValidation(label string, validate func(string) error) (string, error) {
	prompt := promptui.Prompt{
		Label:    label,
		Validate: validate,
	}

	result, err := prompt.Run()
	return result, wrapError(err)
}

// InputOptional prompts for optional text input.
// Returns empty string if user just presses Enter.
func InputOptional(label string) (string, error) {
	prompt := promptui.Prompt{
		Label: label + " (optional)",
	}

	result, err := prompt.Run()
	return result, wrapError(err)
}
