package prompt

import (
	"errors"
	"fmt"
	"strconv"

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

// InputInt prompts for integer input with validation.
func InputInt(label string, defaultValue int) (int, error) {
	prompt := promptui.Prompt{
		Label:   label,
		Default: strconv.Itoa(defaultValue),
		Validate: func(input string) error {
			_, err := strconv.Atoi(input)
			if err != nil {
				return fmt.Errorf("must be a valid integer")
			}
			return nil
		},
	}

	result, err := prompt.Run()
	if err != nil {
		return 0, wrapError(err)
	}

	value, _ := strconv.Atoi(result) // Already validated
	return value, nil
}

// InputPort prompts for a network port with validation (1-65535).
func InputPort(label string, defaultValue int) (int, error) {
	prompt := promptui.Prompt{
		Label:   label,
		Default: strconv.Itoa(defaultValue),
		Validate: func(input string) error {
			port, err := strconv.Atoi(input)
			if err != nil {
				return fmt.Errorf("must be a valid integer")
			}
			if port < 1 || port > 65535 {
				return fmt.Errorf("must be a valid port (1-65535)")
			}
			return nil
		},
	}

	result, err := prompt.Run()
	if err != nil {
		return 0, wrapError(err)
	}

	value, _ := strconv.Atoi(result) // Already validated
	return value, nil
}
