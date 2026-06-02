package orchestrator

import (
	"bytes"
	"strings"
	"testing"
)

func docWith(name string, nsPerOp float64) *Document {
	d := NewDocument("r", "2026-01-02T15:04:05Z", "sha", System{})
	m := Metrics{NsPerOp: nsPerOp, Ops: 100}
	d.Workloads[name] = WorkloadResult{Outcome: OutcomeCompleted, Metrics: &m}
	return d
}

func TestCompareRegression(t *testing.T) {
	base := docWith("w", 100)
	cand := docWith("w", 130) // +30%
	cmps := Compare(base, cand, 10)
	if len(cmps) != 1 {
		t.Fatalf("want 1 comparison, got %d", len(cmps))
	}
	if !cmps[0].Regression {
		t.Errorf("expected regression flagged: %+v", cmps[0])
	}
	if cmps[0].DeltaPct < 29 || cmps[0].DeltaPct > 31 {
		t.Errorf("delta = %v, want ~30", cmps[0].DeltaPct)
	}
	if !HasRegression(cmps) {
		t.Error("HasRegression should be true")
	}
}

func TestCompareWithinThreshold(t *testing.T) {
	cmps := Compare(docWith("w", 100), docWith("w", 105), 10) // +5% < 10%
	if cmps[0].Regression || HasRegression(cmps) {
		t.Errorf("5%% should not flag under 10%% threshold: %+v", cmps[0])
	}
}

func TestCompareMissingWorkload(t *testing.T) {
	base := docWith("a", 100)
	cand := docWith("b", 100)
	cmps := Compare(base, cand, 10)
	if len(cmps) != 2 {
		t.Fatalf("want 2 comparisons (union), got %d", len(cmps))
	}
	var sawMissingCand, sawMissingBase bool
	for _, c := range cmps {
		if c.Name == "a" && c.Missing == "candidate" {
			sawMissingCand = true
		}
		if c.Name == "b" && c.Missing == "baseline" {
			sawMissingBase = true
		}
		if c.Regression {
			t.Errorf("missing workload must not be a regression: %+v", c)
		}
	}
	if !sawMissingCand || !sawMissingBase {
		t.Errorf("missing-side detection failed: %+v", cmps)
	}
}

func TestWriteComparisonAndSummary(t *testing.T) {
	var buf bytes.Buffer
	WriteComparison(&buf, Compare(docWith("w", 100), docWith("w", 200), 10))
	if !strings.Contains(buf.String(), "REGRESSION") {
		t.Errorf("comparison table missing REGRESSION flag:\n%s", buf.String())
	}

	buf.Reset()
	WriteSummary(&buf, docWith("w", 100))
	out := buf.String()
	if !strings.Contains(out, "schema_version=1") || !strings.Contains(out, "WORKLOAD") {
		t.Errorf("summary missing header/version:\n%s", out)
	}
}
