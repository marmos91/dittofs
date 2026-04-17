package repo

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestRepoShow_Table_GroupedSections asserts that the table renderer for a
// repo detail masks S3 credentials (T-06-04-01) and emits grouped fields.
func TestRepoShow_Table_GroupedSections(t *testing.T) {
	now := time.Date(2026, 4, 17, 10, 15, 0, 0, time.UTC)
	r := &apiclient.BackupRepo{
		ID:                "01J00000000000000000000S3",
		Name:              "weekly-s3",
		Kind:              "s3",
		TargetKind:        "metadata_store",
		TargetID:          "01J000000000000000000STORE",
		Schedule:          ptrStr("CRON_TZ=UTC 0 3 * * 0"),
		KeepCount:         ptrInt(4),
		KeepAgeDays:       ptrInt(30),
		EncryptionEnabled: true,
		EncryptionKeyRef:  "env:BACKUP_KEY",
		Config: map[string]any{
			"bucket":            "dittofs-backups",
			"region":            "us-east-1",
			"endpoint":          "https://s3.example.com",
			"prefix":            "meta/",
			"access_key_id":     "AKIAFAKEFAKEFAKE",
			"secret_access_key": "super-secret-value",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	var buf bytes.Buffer
	if err := output.PrintTable(&buf, RepoDetail{r: r}); err != nil {
		t.Fatalf("PrintTable: %v", err)
	}
	got := buf.String()

	// Grouped sections: core fields
	for _, frag := range []string{
		"Name", "weekly-s3",
		"Kind", "s3",
		"Target", "metadata_store/01J000000000000000000STORE",
		"Schedule", "CRON_TZ=UTC 0 3 * * 0",
		"Retention", "count=4 age=30d",
		"Encrypted", "yes",
		"KeyRef", "env:BACKUP_KEY",
		"Bucket", "dittofs-backups",
		"Region", "us-east-1",
		"Endpoint", "https://s3.example.com",
		"Prefix", "meta/",
		"AccessKeyID", "***",
		"SecretAccessKey", "***",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("expected fragment %q in output, got:\n%s", frag, got)
		}
	}

	// Critical security assertion: raw secret must NOT appear in output.
	if strings.Contains(got, "super-secret-value") {
		t.Errorf("raw secret leaked into table output:\n%s", got)
	}
	if strings.Contains(got, "AKIAFAKEFAKEFAKE") {
		t.Errorf("raw access key leaked into table output:\n%s", got)
	}
}

// TestRepoShow_Table_LocalKind verifies the local-kind config renders the
// Path field and nothing else.
func TestRepoShow_Table_LocalKind(t *testing.T) {
	now := time.Date(2026, 4, 17, 10, 15, 0, 0, time.UTC)
	r := &apiclient.BackupRepo{
		ID:         "01J000000000000000000LOCAL",
		Name:       "nightly-local",
		Kind:       "local",
		TargetKind: "metadata_store",
		TargetID:   "01J000000000000000000STORE",
		Schedule:   nil,
		KeepCount:  ptrInt(7),
		Config:     map[string]any{"path": "/var/backups/dittofs"},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	var buf bytes.Buffer
	if err := output.PrintTable(&buf, RepoDetail{r: r}); err != nil {
		t.Fatalf("PrintTable: %v", err)
	}
	got := buf.String()

	for _, frag := range []string{
		"Name", "nightly-local",
		"Kind", "local",
		"Schedule", "-",
		"Retention", "count=7",
		"Encrypted", "no",
		"Path", "/var/backups/dittofs",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("expected fragment %q in output, got:\n%s", frag, got)
		}
	}

	// No KeyRef row when empty
	if strings.Contains(got, "KeyRef") {
		t.Errorf("unexpected KeyRef row for non-encrypted repo:\n%s", got)
	}
}

// TestRepoShow_JSONConfig_PassesThrough documents the decision that
// JSON/YAML output is the raw server response — CLI-side masking is
// table-mode defense-in-depth only. Callers using -o json have a machine-
// readable pipeline and are responsible for their own redaction.
func TestRepoShow_JSONConfig_PassesThrough(t *testing.T) {
	r := &apiclient.BackupRepo{
		Name: "weekly-s3",
		Kind: "s3",
		Config: map[string]any{
			"bucket":            "dittofs-backups",
			"secret_access_key": "super-secret-value",
		},
	}

	var buf bytes.Buffer
	// PrintResource with nil renderer + FormatTable would error; we only
	// exercise that JSON marshalling round-trips the config map. Direct
	// output.PrintJSON is the authoritative path for -o json.
	if err := output.PrintJSON(&buf, r); err != nil {
		t.Fatalf("PrintJSON: %v", err)
	}
	got := buf.String()

	// JSON mode echoes the server's config verbatim — this is intentional
	// per T-06-04-01 disposition note (server chooses redaction policy;
	// the CLI does not second-guess it for machine-readable output).
	if !strings.Contains(got, "super-secret-value") {
		t.Errorf("expected JSON to echo server response verbatim, got:\n%s", got)
	}
}
