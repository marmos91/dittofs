// Package prompt provides interactive terminal prompts for CLI commands.
package prompt

import (
	"fmt"
	"strings"

	"github.com/manifoldco/promptui"
)

// Confirm prompts the user for yes/no confirmation.
// Returns true if the user confirms, false otherwise.
// Returns ErrAborted if the user presses Ctrl+C.
func Confirm(label string, defaultYes bool) (bool, error) {
	defaultStr := "y/N"
	if defaultYes {
		defaultStr = "Y/n"
	}

	prompt := promptui.Prompt{
		Label:     fmt.Sprintf("%s [%s]", label, defaultStr),
		IsConfirm: true,
		Default:   "",
	}

	result, err := prompt.Run()
	if err != nil {
		// Ctrl+C should abort
		if err == promptui.ErrInterrupt {
			return false, ErrAborted
		}
		// promptui returns ErrAbort for "n" response
		if err == promptui.ErrAbort {
			return false, nil
		}
		// Empty input uses default
		if result == "" {
			return defaultYes, nil
		}
		return false, err
	}

	return strings.ToLower(result) == "y" || strings.ToLower(result) == "yes", nil
}

// ConfirmDanger prompts for confirmation of a dangerous operation.
// Requires typing the confirmation word to proceed.
// Returns ErrAborted if the user presses Ctrl+C.
func ConfirmDanger(label, confirmWord string) (bool, error) {
	prompt := promptui.Prompt{
		Label: fmt.Sprintf("%s (type '%s' to confirm)", label, confirmWord),
		Validate: func(input string) error {
			if input != confirmWord {
				return fmt.Errorf("type '%s' to confirm", confirmWord)
			}
			return nil
		},
	}

	result, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return false, ErrAborted
		}
		if err == promptui.ErrAbort {
			return false, nil
		}
		return false, err
	}

	return result == confirmWord, nil
}

// ConfirmWithForce returns true immediately if force is true,
// otherwise prompts for confirmation.
func ConfirmWithForce(label string, force bool) (bool, error) {
	if force {
		return true, nil
	}
	return Confirm(label, false)
}
