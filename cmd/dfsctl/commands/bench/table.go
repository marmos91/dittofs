package bench

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/bench"
)

// ResultTable renders a single benchmark result as a table.
type ResultTable struct {
	Result *bench.Result
}

// Headers implements output.TableRenderer.
func (t ResultTable) Headers() []string {
	return []string{"WORKLOAD", "THROUGHPUT", "IOPS", "OPS/SEC", "P50", "P95", "P99"}
}

// Rows implements output.TableRenderer.
func (t ResultTable) Rows() [][]string {
	order := bench.AllWorkloads()
	rows := make([][]string, 0, len(order))

	for _, w := range order {
		wr, ok := t.Result.Workloads[w]
		if !ok {
			continue
		}
		rows = append(rows, []string{
			string(wr.Workload),
			formatThroughput(wr.ThroughputMBps),
			formatIOPS(wr.IOPS),
			formatOps(wr.OpsPerSec),
			formatLatency(wr.LatencyP50Us),
			formatLatency(wr.LatencyP95Us),
			formatLatency(wr.LatencyP99Us),
		})
	}

	return rows
}

// CompareTable renders a side-by-side comparison of multiple results.
type CompareTable struct {
	Results []*bench.Result
}

// Headers implements output.TableRenderer.
func (t CompareTable) Headers() []string {
	headers := []string{"WORKLOAD", "METRIC"}
	for _, r := range t.Results {
		label := r.System
		if label == "" {
			label = r.Path
		}
		headers = append(headers, label)
	}
	return headers
}

// Rows implements output.TableRenderer.
func (t CompareTable) Rows() [][]string {
	var rows [][]string

	for _, w := range bench.AllWorkloads() {
		metrics := []struct {
			name string
			fn   func(*bench.WorkloadResult) string
		}{
			{"throughput", func(wr *bench.WorkloadResult) string { return formatThroughput(wr.ThroughputMBps) }},
			{"iops", func(wr *bench.WorkloadResult) string { return formatIOPS(wr.IOPS) }},
			{"ops/sec", func(wr *bench.WorkloadResult) string { return formatOps(wr.OpsPerSec) }},
			{"p50", func(wr *bench.WorkloadResult) string { return formatLatency(wr.LatencyP50Us) }},
			{"p99", func(wr *bench.WorkloadResult) string { return formatLatency(wr.LatencyP99Us) }},
		}

		for _, m := range metrics {
			row := []string{string(w), m.name}
			hasValue := false

			for _, r := range t.Results {
				wr, ok := r.Workloads[w]
				if !ok {
					row = append(row, "-")
					continue
				}
				val := m.fn(wr)
				if val != "-" {
					hasValue = true
				}
				row = append(row, val)
			}

			if hasValue {
				rows = append(rows, row)
			}
		}
	}

	return rows
}

func formatThroughput(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f MB/s", v)
}

func formatIOPS(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

func formatOps(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

func formatLatency(us float64) string {
	if us == 0 {
		return "-"
	}
	if us >= 1000 {
		return fmt.Sprintf("%.1f ms", us/1000)
	}
	return fmt.Sprintf("%.0f us", us)
}
