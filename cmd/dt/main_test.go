package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/plan"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRoot(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// dt plan --json gives every cell a name, a place and a limit.
func TestPlanJSON(t *testing.T) {
	out, err := run(t, "plan", "--tier", "pr", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tier  string      `json:"tier"`
		Cells []plan.Cell `json:"cells"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tier != "pr" || len(got.Cells) == 0 {
		t.Fatalf("plan: %+v", got)
	}
	for _, c := range got.Cells {
		if c.Name == "" || c.Place == "" || c.TimeoutMinutes == 0 {
			t.Errorf("incomplete cell %+v", c)
		}
	}
}

// --shard I/N keeps every N-th cell from the I-th, and refuses a bad spec.
func TestPlanShard(t *testing.T) {
	all, _ := run(t, "plan", "--tier", "pr")
	one, err := run(t, "plan", "--tier", "pr", "--shard", "1/2")
	if err != nil {
		t.Fatal(err)
	}
	if lines(one) >= lines(all) {
		t.Errorf("shard 1/2 has %d lines, the whole plan %d", lines(one), lines(all))
	}
	if _, err := run(t, "plan", "--tier", "pr", "--shard", "3/2"); err == nil {
		t.Error("--shard 3/2 accepted")
	}
}

// A usage error is an error, which main turns into exit status 3.
func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"plan"}, {"plan", "--tier", "weekly"}, {"list", "--tier", "weekly"}} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("dt %s: no error", strings.Join(args, " "))
		}
	}
}

func TestListJSON(t *testing.T) {
	out, err := run(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Suites []struct{ Name, Tier string } `json:"suites"`
		Groups map[string][]string           `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Suites) == 0 || len(got.Groups["conformance"]) == 0 {
		t.Fatalf("list: %+v", got)
	}
}

func lines(s string) int { return strings.Count(s, "\n") }
