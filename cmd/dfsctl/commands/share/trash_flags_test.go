package share

import (
	"net/http"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// Refs #190 — dfsctl flag plumbing for the per-share recycle bin on
// `share create` / `share edit`, plus the `share show` rendering.

// TestCreateCmd_Trash_FlagsMapToRequest drives `share create` with all five
// trash flags set and asserts the wire payload carries each field.
func TestCreateCmd_Trash_FlagsMapToRequest(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetCreateFlags()
	createName = "/x"
	createMetadata = "meta"
	createLocal = "bs"
	for k, v := range map[string]string{
		"enable-trash":                  "true",
		"trash-retention-days":          "7",
		"trash-restrict-empty-to-admin": "true",
		"trash-max-size":                "1024",
		"trash-exclude":                 "*.tmp,~$*",
	} {
		if err := createCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("Flags.Set(%q): %v", k, err)
		}
	}

	_ = captureStdout(t, func() {
		if err := runCreate(createCmd, nil); err != nil {
			t.Fatalf("runCreate: %v", err)
		}
	})

	if s.lastVerb != http.MethodPost {
		t.Fatalf("verb = %q, want POST", s.lastVerb)
	}
	body := string(s.lastBody)
	for _, want := range []string{
		`"trash_enabled":true`,
		`"trash_retention_days":7`,
		`"trash_restrict_to_admin":true`,
		`"trash_max_bytes":1024`,
		`"trash_exclude_patterns":["*.tmp","~$*"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("create wire body missing %s; got %s", want, body)
		}
	}
}

// TestCreateCmd_Trash_UnsetOmitsFields confirms trash fields are omitted from
// the wire payload when the operator does not pass them — the server applies
// its own defaults.
func TestCreateCmd_Trash_UnsetOmitsFields(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetCreateFlags()
	createName = "/x"
	createMetadata = "meta"
	createLocal = "bs"

	_ = captureStdout(t, func() {
		if err := runCreate(createCmd, nil); err != nil {
			t.Fatalf("runCreate: %v", err)
		}
	})

	body := string(s.lastBody)
	for _, field := range []string{"trash_enabled", "trash_retention_days", "trash_restrict_to_admin", "trash_max_bytes", "trash_exclude_patterns"} {
		if strings.Contains(body, field) {
			t.Errorf("create wire body must omit %s when flag unset; got %s", field, body)
		}
	}
}

// TestEditCmd_Trash_FlagsMapToRequest drives `share edit` with the trash flags
// and asserts the wire payload.
func TestEditCmd_Trash_FlagsMapToRequest(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetEditFlags()
	for k, v := range map[string]string{
		"enable-trash":                  "false",
		"trash-retention-days":          "0",
		"trash-restrict-empty-to-admin": "true",
		"trash-max-size":                "0",
		"trash-exclude":                 "*.bak",
	} {
		if err := editCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("Flags.Set(%q): %v", k, err)
		}
	}

	_ = captureStdout(t, func() {
		if err := runEdit(editCmd, []string{"x"}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})

	if s.lastVerb != http.MethodPut {
		t.Fatalf("verb = %q, want PUT", s.lastVerb)
	}
	body := string(s.lastBody)
	for _, want := range []string{
		`"trash_enabled":false`,
		`"trash_retention_days":0`,
		`"trash_restrict_to_admin":true`,
		`"trash_max_bytes":0`,
		`"trash_exclude_patterns":["*.bak"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit wire body missing %s; got %s", want, body)
		}
	}
}

// TestEditCmd_Trash_InvalidEnableTrash rejects non-bool values and fires no
// HTTP request.
func TestEditCmd_Trash_InvalidEnableTrash(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetEditFlags()
	editEnableTrash = "maybe"
	if err := editCmd.Flags().Set("enable-trash", "maybe"); err != nil {
		t.Fatalf("Flags.Set: %v", err)
	}

	var runErr error
	_ = captureStdout(t, func() {
		runErr = runEdit(editCmd, []string{"x"})
	})
	if runErr == nil {
		t.Fatalf("expected error for --enable-trash=maybe, got nil")
	}
	if !strings.Contains(runErr.Error(), "--enable-trash") {
		t.Errorf("error %q should reference --enable-trash", runErr.Error())
	}
	if s.lastVerb != "" {
		t.Errorf("expected no HTTP request on parse error; got %s %s", s.lastVerb, s.lastPath)
	}
}

// TestShareDetail_Rows_TrashEnabled asserts the detail table includes the
// trash rows when the bin is enabled.
func TestShareDetail_Rows_TrashEnabled(t *testing.T) {
	sd := ShareDetail{share: &apiclient.Share{
		Name:                 "/alice",
		TrashEnabled:         true,
		TrashRetentionDays:   30,
		TrashRestrictToAdmin: true,
		TrashMaxBytes:        1 << 30,
		TrashExcludePatterns: []string{"*.tmp", "*.swp"},
	}}
	rows := sd.Rows()
	got := map[string]string{}
	for _, r := range rows {
		if len(r) >= 2 {
			got[r[0]] = r[1]
		}
	}
	want := map[string]string{
		"Trash Enabled":                 "true",
		"Trash Retention (days)":        "30",
		"Trash Restrict Empty To Admin": "true",
		"Trash Exclude":                 "*.tmp, *.swp",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %q = %q, want %q (rows=%v)", k, got[k], v, rows)
		}
	}
	if _, ok := got["Trash Max Size"]; !ok {
		t.Errorf("missing Trash Max Size row; rows=%v", rows)
	}
}

// TestShareDetail_Rows_TrashDisabled asserts only the enabled state shows when
// the bin is disabled (detail rows are suppressed).
func TestShareDetail_Rows_TrashDisabled(t *testing.T) {
	sd := ShareDetail{share: &apiclient.Share{Name: "/archive", TrashEnabled: false}}
	rows := sd.Rows()
	sawEnabled := false
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		switch r[0] {
		case "Trash Enabled":
			sawEnabled = true
			if r[1] != "false" {
				t.Errorf("Trash Enabled = %q, want \"false\"", r[1])
			}
		case "Trash Retention (days)", "Trash Restrict Empty To Admin", "Trash Max Size", "Trash Exclude":
			t.Errorf("detail row %q must be suppressed when trash disabled", r[0])
		}
	}
	if !sawEnabled {
		t.Errorf("Trash Enabled row missing from disabled-share output: %v", rows)
	}
}
