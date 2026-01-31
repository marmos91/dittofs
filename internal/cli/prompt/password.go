package prompt

import (
	"errors"
	"fmt"

	"github.com/manifoldco/promptui"
)

// ErrPasswordMismatch indicates passwords don't match.
var ErrPasswordMismatch = errors.New("passwords do not match")

// Password prompts for a password input with masking.
func Password(label string) (string, error) {
	prompt := promptui.Prompt{
		Label: label,
		Mask:  '*',
	}

	result, err := prompt.Run()
	return result, wrapError(err)
}

// PasswordWithValidation prompts for a password with minimum length validation.
func PasswordWithValidation(label string, minLength int) (string, error) {
	prompt := promptui.Prompt{
		Label: label,
		Mask:  '*',
		Validate: func(input string) error {
			if len(input) < minLength {
				return fmt.Errorf("password must be at least %d characters", minLength)
			}
			return nil
		},
	}

	result, err := prompt.Run()
	return result, wrapError(err)
}

// PasswordWithConfirmation prompts for a password and confirmation.
// Returns error if passwords don't match.
func PasswordWithConfirmation(label, confirmLabel string, minLength int) (string, error) {
	// First password prompt
	password, err := PasswordWithValidation(label, minLength)
	if err != nil {
		return "", err
	}

	// Confirmation prompt
	confirm, err := Password(confirmLabel)
	if err != nil {
		return "", err
	}

	if password != confirm {
		return "", ErrPasswordMismatch
	}

	return password, nil
}

// NewPassword prompts for a new password with confirmation.
// Uses default labels and minimum length of 8.
func NewPassword() (string, error) {
	return PasswordWithConfirmation("Password", "Confirm password", 8)
}
