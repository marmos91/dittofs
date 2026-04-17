package share

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/cli/output"
)

// TestShareList_Headers_IncludesEnabled asserts the table headers grew an
// ENABLED column (D-28 surfacing).
func TestShareList_Headers_IncludesEnabled(t *testing.T) {
	sl := ShareList{}
	headers := sl.Headers()
	found := false
	for _, h := range headers {
		if h == "ENABLED" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ENABLED column missing from headers: %v", headers)
	}
}

// TestShareList_Row_RendersEnabledYes asserts a row with Enabled=true
// renders "yes" in the ENABLED column.
func TestShareList_Row_RendersEnabledYes(t *testing.T) {
	sl := ShareList{
		{Name: "/alice", Enabled: true},
	}
	rows := sl.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	last := rows[0][len(rows[0])-1]
	if last != "yes" {
		t.Errorf("last column = %q, want \"yes\"", last)
	}
}

// TestShareList_Row_RendersEnabledDash asserts a row with Enabled=false
// renders "-" in the ENABLED column (matches existing PINNED-style rendering).
func TestShareList_Row_RendersEnabledDash(t *testing.T) {
	sl := ShareList{
		{Name: "/archive", Enabled: false},
	}
	rows := sl.Rows()
	last := rows[0][len(rows[0])-1]
	if last != "-" {
		t.Errorf("last column = %q, want \"-\"", last)
	}
}

// TestShareList_Table_IncludesEnabledHeaderAndRow renders the full table and
// asserts the formatted output contains both the ENABLED header and a yes/-
// cell.
func TestShareList_Table_IncludesEnabledHeaderAndRow(t *testing.T) {
	sl := ShareList{
		{Name: "/alice", Enabled: true},
		{Name: "/archive", Enabled: false},
	}
	var buf bytes.Buffer
	if err := output.PrintTable(&buf, sl); err != nil {
		t.Fatalf("PrintTable: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "ENABLED") {
		t.Errorf("table output missing ENABLED header: %s", got)
	}
	if !strings.Contains(got, "yes") {
		t.Errorf("table output missing \"yes\" cell: %s", got)
	}
}

// TestShareList_JSON_IncludesEnabledField marshals a shareRow as JSON and
// confirms the `enabled` tag is present (D-28 -o json surfacing).
func TestShareList_JSON_IncludesEnabledField(t *testing.T) {
	sl := ShareList{
		{Name: "/alice", Enabled: true},
	}
	b, err := json.Marshal(sl)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"enabled":true`) {
		t.Errorf("json output missing \"enabled\":true, got %s", got)
	}
}
