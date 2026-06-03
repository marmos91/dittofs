package orchestrator

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// Comparison is the per-workload delta between a baseline and a candidate run.
type Comparison struct {
	Name string
	// BaseNsPerOp / NewNsPerOp are the ns/op for each side. A workload missing
	// from one side has its corresponding value left at 0 and Missing set.
	BaseNsPerOp float64
	NewNsPerOp  float64
	// DeltaPct is the percentage change in ns/op from baseline to candidate
	// (positive = slower = regression). 0 when baseline ns/op is 0.
	DeltaPct float64
	// Regression is true when DeltaPct exceeds the caller's threshold.
	Regression bool
	// Missing names a side that lacked this workload ("baseline" or
	// "candidate"); empty when both have it.
	Missing string
}

// Compare diffs two result documents by workload, flagging any workload whose
// ns/op regressed by more than thresholdPct percent. Workloads present in only
// one document are reported with Missing set and never flagged as regressions
// (there is nothing to compare). The result is sorted by name for stable output.
func Compare(baseline, candidate *Document, thresholdPct float64) []Comparison {
	names := map[string]struct{}{}
	for n := range baseline.Workloads {
		names[n] = struct{}{}
	}
	for n := range candidate.Workloads {
		names[n] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	out := make([]Comparison, 0, len(sorted))
	for _, n := range sorted {
		b, hasB := baseline.Workloads[n]
		c, hasC := candidate.Workloads[n]
		cmp := Comparison{Name: n}
		switch {
		case !hasB:
			cmp.Missing = "baseline"
		case !hasC:
			cmp.Missing = "candidate"
		default:
			if b.Metrics != nil {
				cmp.BaseNsPerOp = b.Metrics.NsPerOp
			}
			if c.Metrics != nil {
				cmp.NewNsPerOp = c.Metrics.NsPerOp
			}
			if cmp.BaseNsPerOp > 0 {
				cmp.DeltaPct = (cmp.NewNsPerOp - cmp.BaseNsPerOp) / cmp.BaseNsPerOp * 100
				cmp.Regression = cmp.DeltaPct > thresholdPct
			}
		}
		out = append(out, cmp)
	}
	return out
}

// HasRegression reports whether any comparison flagged a regression.
func HasRegression(cmps []Comparison) bool {
	for _, c := range cmps {
		if c.Regression {
			return true
		}
	}
	return false
}

// WriteComparison renders the comparisons as an aligned table.
func WriteComparison(w io.Writer, cmps []Comparison) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKLOAD\tBASE_NS/OP\tNEW_NS/OP\tDELTA%\tFLAG")
	for _, c := range cmps {
		flag := ""
		switch {
		case c.Missing != "":
			flag = "missing-in-" + c.Missing
		case c.Regression:
			flag = "REGRESSION"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%+.1f\t%s\n", c.Name, c.BaseNsPerOp, c.NewNsPerOp, c.DeltaPct, flag)
	}
	_ = tw.Flush()
}

// WriteSummary renders a document's per-workload metrics as an aligned table —
// a quick human-readable companion to the JSON output.
func WriteSummary(w io.Writer, doc *Document) {
	_, _ = fmt.Fprintf(w, "run_id=%s schema_version=%d outcome=%s git_sha=%s\n",
		doc.RunID, doc.SchemaVersion, doc.Outcome, doc.GitSHA)
	if doc.AbortReason != "" {
		_, _ = fmt.Fprintf(w, "abort_reason=%s\n", doc.AbortReason)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKLOAD\tOUTCOME\tOPS\tFAILED\tNS/OP\tP50µs\tP95µs\tP99µs\tMB/SEC")
	for _, name := range sortedKeys(doc.Workloads) {
		r := doc.Workloads[name]
		if r.Metrics == nil {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t-\t-\t-\n", name, r.Outcome)
			continue
		}
		mbps := r.Metrics.BytesPerSec / (1024 * 1024)
		p50, p95, p99 := "-", "-", "-"
		if l := r.Metrics.Latency; l != nil {
			p50 = fmt.Sprintf("%.1f", float64(l.P50Ns)/1000)
			p95 = fmt.Sprintf("%.1f", float64(l.P95Ns)/1000)
			p99 = fmt.Sprintf("%.1f", float64(l.P99Ns)/1000)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.1f\t%s\t%s\t%s\t%.2f\n",
			name, r.Outcome, r.Metrics.Ops, r.Metrics.OpCounts.Failed,
			r.Metrics.NsPerOp, p50, p95, p99, mbps)
	}
	_ = tw.Flush()
}

func sortedKeys(m map[string]WorkloadResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
