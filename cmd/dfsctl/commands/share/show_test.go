package share

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestShareEnabledString maps the boolean to yes/no per D-28.
func TestShareEnabledString(t *testing.T) {
	if got := shareEnabledString(true); got != "yes" {
		t.Errorf("shareEnabledString(true) = %q, want \"yes\"", got)
	}
	if got := shareEnabledString(false); got != "no" {
		t.Errorf("shareEnabledString(false) = %q, want \"no\"", got)
	}
}

// TestShareDetail_Rows_IncludesEnabled asserts the detail table rows contain
// an "Enabled" row with the yes/no rendering.
func TestShareDetail_Rows_IncludesEnabled_Yes(t *testing.T) {
	sd := ShareDetail{share: &apiclient.Share{Name: "/alice", Enabled: true}}
	rows := sd.Rows()
	found := false
	for _, r := range rows {
		if len(r) >= 2 && r[0] == "Enabled" {
			found = true
			if r[1] != "yes" {
				t.Errorf("Enabled row value = %q, want \"yes\"", r[1])
			}
		}
	}
	if !found {
		t.Errorf("Enabled row missing from ShareDetail.Rows() output: %v", rows)
	}
}

func TestShareDetail_Rows_IncludesEnabled_No(t *testing.T) {
	sd := ShareDetail{share: &apiclient.Share{Name: "/archive", Enabled: false}}
	rows := sd.Rows()
	found := false
	for _, r := range rows {
		if len(r) >= 2 && r[0] == "Enabled" {
			found = true
			if r[1] != "no" {
				t.Errorf("Enabled row value = %q, want \"no\"", r[1])
			}
		}
	}
	if !found {
		t.Errorf("Enabled row missing from ShareDetail.Rows() output")
	}
}

// TestShareJSONMarshal_IncludesEnabled verifies that marshalling a *Share via
// json.Marshal (the path -o json takes via PrintResource) surfaces the
// "enabled" field unconditionally.
func TestShareJSONMarshal_IncludesEnabled(t *testing.T) {
	s := &apiclient.Share{Name: "/alice", Enabled: true}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"enabled":true`) {
		t.Errorf(`json output missing "enabled":true, got %s`, got)
	}

	s2 := &apiclient.Share{Name: "/archive", Enabled: false}
	b2, err := json.Marshal(s2)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	got2 := string(b2)
	if !strings.Contains(got2, `"enabled":false`) {
		t.Errorf(`json output missing "enabled":false (omitempty regression?), got %s`, got2)
	}
}

// TestShareTree_AllVerbsDiscoverable asserts every D-35 canonical verb is
// registered as a child of the share parent, including the new Phase-6
// disable/enable and the inherited permission sub-tree.
func TestShareTree_AllVerbsDiscoverable(t *testing.T) {
	verbs := []string{
		"list", "create",
		"show", "edit", "delete",
		"mount", "unmount",
		"disable", "enable",
		"permission",
		"list-mounts",
	}

	children := Cmd.Commands()
	have := make(map[string]bool, len(children))
	for _, c := range children {
		have[c.Name()] = true
	}

	for _, v := range verbs {
		if !have[v] {
			t.Errorf("share parent missing subcommand %q; registered: %v", v, keys(have))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
