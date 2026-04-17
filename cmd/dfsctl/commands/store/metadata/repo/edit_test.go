package repo

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// resetEditFlags resets package-level edit flag state between tests.
func resetEditFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		editSchedule = ""
		editKeepCount = 0
		editKeepAgeDays = 0
		editEncryption = ""
		editEncryptionKeyRef = ""
	})
}

// newEditTestCmd builds a fresh cobra command with the same flag set as
// editCmd so test code can call Flags().Changed("...") without touching
// the globally registered command. Each call pre-seeds the package-level
// flag variables just like cobra would.
func newEditTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "edit"}
	c.Flags().StringVar(&editSchedule, "schedule", "", "")
	c.Flags().IntVar(&editKeepCount, "keep-count", 0, "")
	c.Flags().IntVar(&editKeepAgeDays, "keep-age-days", 0, "")
	c.Flags().StringVar(&editEncryption, "encryption", "", "")
	c.Flags().StringVar(&editEncryptionKeyRef, "encryption-key-ref", "", "")
	return c
}

func TestRepoEdit_OnlyChangedFlagsSet(t *testing.T) {
	resetEditFlags(t)

	c := newEditTestCmd()
	// Simulate operator passing only --keep-count 14
	if err := c.ParseFlags([]string{"--keep-count", "14"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	req, err := buildEditRequest(c)
	if err != nil {
		t.Fatalf("buildEditRequest: %v", err)
	}
	if req.KeepCount == nil || *req.KeepCount != 14 {
		t.Errorf("KeepCount = %v, want 14", req.KeepCount)
	}
	if req.Schedule != nil {
		t.Errorf("Schedule should be nil, got %v", req.Schedule)
	}
	if req.KeepAgeDays != nil {
		t.Errorf("KeepAgeDays should be nil, got %v", req.KeepAgeDays)
	}
	if req.EncryptionEnabled != nil {
		t.Errorf("EncryptionEnabled should be nil, got %v", req.EncryptionEnabled)
	}
	if req.EncryptionKeyRef != nil {
		t.Errorf("EncryptionKeyRef should be nil, got %v", req.EncryptionKeyRef)
	}
}

func TestRepoEdit_NoFlags_Errors(t *testing.T) {
	resetEditFlags(t)

	c := newEditTestCmd()
	if err := c.ParseFlags([]string{}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	_, err := buildEditRequest(c)
	if err == nil {
		t.Fatal("expected error when no edit flags changed")
	}
	if !strings.Contains(err.Error(), "No fields to update") {
		t.Errorf("error = %q, want message containing 'No fields to update'", err.Error())
	}
}

func TestRepoEdit_ClearSchedule_EmptyStringIsChanged(t *testing.T) {
	// Passing --schedule "" is a deliberate clear: partial patch should
	// send req.Schedule = &"" to the server, not skip the field.
	resetEditFlags(t)

	c := newEditTestCmd()
	if err := c.ParseFlags([]string{"--schedule", ""}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	req, err := buildEditRequest(c)
	if err != nil {
		t.Fatalf("buildEditRequest: %v", err)
	}
	if req.Schedule == nil {
		t.Fatal("Schedule should be set (pointer to empty string), got nil")
	}
	if *req.Schedule != "" {
		t.Errorf("Schedule = %q, want empty", *req.Schedule)
	}
}

func TestRepoEdit_ClearKeepCount_ZeroIsChanged(t *testing.T) {
	// Passing --keep-count 0 is a deliberate clear.
	resetEditFlags(t)

	c := newEditTestCmd()
	if err := c.ParseFlags([]string{"--keep-count", "0"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	req, err := buildEditRequest(c)
	if err != nil {
		t.Fatalf("buildEditRequest: %v", err)
	}
	if req.KeepCount == nil {
		t.Fatal("KeepCount should be set (pointer to 0), got nil")
	}
	if *req.KeepCount != 0 {
		t.Errorf("KeepCount = %d, want 0", *req.KeepCount)
	}
}

func TestRepoEdit_EncryptionToggle(t *testing.T) {
	resetEditFlags(t)

	c := newEditTestCmd()
	if err := c.ParseFlags([]string{"--encryption", "on", "--encryption-key-ref", "env:NEW_KEY"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	req, err := buildEditRequest(c)
	if err != nil {
		t.Fatalf("buildEditRequest: %v", err)
	}
	if req.EncryptionEnabled == nil || !*req.EncryptionEnabled {
		t.Errorf("EncryptionEnabled should be true, got %v", req.EncryptionEnabled)
	}
	if req.EncryptionKeyRef == nil || *req.EncryptionKeyRef != "env:NEW_KEY" {
		t.Errorf("EncryptionKeyRef = %v, want env:NEW_KEY", req.EncryptionKeyRef)
	}
}

func TestRepoEdit_EncryptionOff(t *testing.T) {
	resetEditFlags(t)

	c := newEditTestCmd()
	if err := c.ParseFlags([]string{"--encryption", "off"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	req, err := buildEditRequest(c)
	if err != nil {
		t.Fatalf("buildEditRequest: %v", err)
	}
	if req.EncryptionEnabled == nil || *req.EncryptionEnabled {
		t.Errorf("EncryptionEnabled should be false, got %v", req.EncryptionEnabled)
	}
}
