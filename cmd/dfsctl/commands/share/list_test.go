package share

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/cli/output"
)

// TestShareList_Headers_IncludesEnabled asserts the table headers grew an
// ENABLED column.
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
	got := rows[0][columnIndex(t, sl.Headers(), "ENABLED")]
	if got != "yes" {
		t.Errorf("ENABLED column = %q, want \"yes\"", got)
	}
}

// TestShareList_Row_RendersEnabledDash asserts a row with Enabled=false
// renders "-" in the ENABLED column (matches existing PINNED-style rendering).
func TestShareList_Row_RendersEnabledDash(t *testing.T) {
	sl := ShareList{
		{Name: "/archive", Enabled: false},
	}
	rows := sl.Rows()
	got := rows[0][columnIndex(t, sl.Headers(), "ENABLED")]
	if got != "-" {
		t.Errorf("ENABLED column = %q, want \"-\"", got)
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
// confirms the `enabled` tag is present in -o json output.
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

// columnIndex returns the position of the named column, so assertions survive
// new columns being appended to the table.
func columnIndex(t *testing.T, headers []string, name string) int {
	t.Helper()
	for i, h := range headers {
		if h == name {
			return i
		}
	}
	t.Fatalf("column %q missing from headers: %v", name, headers)
	return -1
}

// A share the server refused to serve must be visible in the table, and a
// share whose status the server did not report must not render an empty cell.
func TestShareList_Row_RendersStatus(t *testing.T) {
	sl := ShareList{
		{Name: "/refused", Enabled: true, Status: "unhealthy"},
		{Name: "/nostatus", Enabled: true},
	}
	rows := sl.Rows()
	idx := columnIndex(t, sl.Headers(), "STATUS")

	if got := rows[0][idx]; got != "unhealthy" {
		t.Errorf("STATUS column = %q, want \"unhealthy\"", got)
	}
	if got := rows[1][idx]; got != "-" {
		t.Errorf("absent status rendered as %q, want \"-\"", got)
	}
}
