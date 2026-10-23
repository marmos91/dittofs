package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// fioIO is what fio's JSON report gives for one direction of a job's I/O.
type fioIO struct {
	IOBytes int64   `json:"io_bytes"`
	BW      float64 `json:"bw_bytes"` // bytes per second
	IOPS    float64 `json:"iops"`
	Lat     struct {
		Mean float64 `json:"mean"`
	} `json:"lat_ns"`
}

// readFio reads the JSON report fio wrote to path: for each job, its
// bandwidth, IOPS and mean latency in each direction it did I/O in, by
// JOB/DIRECTION/NAME, as seq-write/write/bw_bytes, and why each job whose
// error isn't 0 failed, a failed verify included.
//
// decision: a job's numbers are keyed by its name. The workloads report each
// job's threads as one group (group_reporting); of jobs that share a name,
// the last one's numbers are kept.
func readFio(path string) (map[string]float64, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	// A job that continues on errors logs them to the report's file, before
	// the report.
	var rep struct {
		Jobs []struct {
			Name  string `json:"jobname"`
			Error int    `json:"error"`
			Read  fioIO  `json:"read"`
			Write fioIO  `json:"write"`
			Trim  fioIO  `json:"trim"`
		} `json:"jobs"`
	}
	if i := bytes.IndexByte(data, '{'); i < 0 || json.NewDecoder(bytes.NewReader(data[i:])).Decode(&rep) != nil || len(rep.Jobs) == 0 {
		return nil, nil, fmt.Errorf("%s holds no fio report", filepath.Base(path))
	}
	numbers := map[string]float64{}
	var failed []string
	for _, j := range rep.Jobs {
		if j.Error != 0 {
			failed = append(failed, fmt.Sprintf("job %s: error %d (%v)", j.Name, j.Error, syscall.Errno(j.Error)))
		}
		for dir, io := range map[string]fioIO{"read": j.Read, "write": j.Write, "trim": j.Trim} {
			if io.IOBytes == 0 {
				continue
			}
			key := j.Name + "/" + dir + "/"
			numbers[key+"bw_bytes"], numbers[key+"iops"], numbers[key+"lat_ns_mean"] = io.BW, io.IOPS, io.Lat.Mean
		}
	}
	return numbers, failed, nil
}
