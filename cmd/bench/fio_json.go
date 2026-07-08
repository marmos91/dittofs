package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// fioOutput is the subset of `fio --output-format=json` we consume. fio has no
// canonical Go type, so we decode only the fields the scorecard needs.
type fioOutput struct {
	Jobs []fioJob `json:"jobs"`
}

type fioJob struct {
	Jobname    string     `json:"jobname"`
	Error      int64      `json:"error"`
	JobRuntime int64      `json:"job_runtime"` // milliseconds
	Read       fioDirStat `json:"read"`
	Write      fioDirStat `json:"write"`
}

type fioDirStat struct {
	IOBytes  int64    `json:"io_bytes"`
	BWBytes  int64    `json:"bw_bytes"` // bytes/sec
	IOPS     float64  `json:"iops"`
	TotalIOS int64    `json:"total_ios"`
	ClatNS   fioLatNS `json:"clat_ns"`
}

type fioLatNS struct {
	Mean       float64            `json:"mean"`
	Percentile map[string]float64 `json:"percentile"`
}

const mib = 1 << 20

// parseFioJSON turns one fio JSON report into a CellResult's metric fields. It
// combines the read and write directions (a mixed workload populates both; a
// pure read/write leaves the other side zero) and takes latency percentiles
// from whichever direction did more ops — the workload's headline op.
func parseFioJSON(data []byte) (CellResult, error) {
	// fio prepends free-text "note:" lines to stdout (and even to --output
	// files) before the JSON object, so scrape from the first brace.
	if i := bytes.IndexByte(data, '{'); i > 0 {
		data = data[i:]
	}
	var out fioOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return CellResult{}, fmt.Errorf("decode fio json: %w", err)
	}
	if len(out.Jobs) == 0 {
		return CellResult{}, fmt.Errorf("fio json has no jobs")
	}
	// group_reporting collapses to a single job entry.
	j := out.Jobs[0]

	var r CellResult
	r.ThroughputMBps = float64(j.Read.BWBytes+j.Write.BWBytes) / mib
	r.IOPS = j.Read.IOPS + j.Write.IOPS
	r.TotalOps = j.Read.TotalIOS + j.Write.TotalIOS
	r.TotalBytes = j.Read.IOBytes + j.Write.IOBytes
	r.Errors = j.Error
	r.DurationS = float64(j.JobRuntime) / 1000

	headline := j.Read
	if j.Write.TotalIOS > j.Read.TotalIOS {
		headline = j.Write
	}
	r.LatencyAvgUs = headline.ClatNS.Mean / 1000
	r.LatencyP50Us = headline.ClatNS.Percentile["50.000000"] / 1000
	r.LatencyP95Us = headline.ClatNS.Percentile["95.000000"] / 1000
	r.LatencyP99Us = headline.ClatNS.Percentile["99.000000"] / 1000
	return r, nil
}
