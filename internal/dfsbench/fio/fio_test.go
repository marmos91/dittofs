package fio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckTarget_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	if err := CheckTarget(dir); err != nil {
		t.Fatalf("CheckTarget should mkdir -p a fresh path: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("target not created as a dir: err=%v", err)
	}
}

func TestCheckTarget_ExistingFileErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckTarget(file); err == nil {
		t.Fatal("CheckTarget should error when the target is an existing file")
	}
}

// a trimmed but real-shaped fio --output-format=json report (rand-read-4k).
const fioRandReadJSON = `{
  "jobs": [
    {
      "jobname": "rand-read-4k",
      "error": 0,
      "job_runtime": 3000,
      "read": {
        "io_bytes": 104857600,
        "bw_bytes": 34952533,
        "iops": 8533.2,
        "total_ios": 25600,
        "clat_ns": { "mean": 112000, "percentile": {
          "50.000000": 98000, "95.000000": 1870000, "99.000000": 2310000 } }
      },
      "write": { "io_bytes": 0, "bw_bytes": 0, "iops": 0, "total_ios": 0,
        "clat_ns": { "mean": 0, "percentile": {} } }
    }
  ]
}`

func TestParseFioJSON_RandRead(t *testing.T) {
	r, err := parseFioJSON([]byte(fioRandReadJSON))
	if err != nil {
		t.Fatal(err)
	}
	if r.IOPS != 8533.2 {
		t.Errorf("iops=%v want 8533.2", r.IOPS)
	}
	if want := 34952533.0 / Mib; r.ThroughputMBps != want {
		t.Errorf("mbps=%v want %v", r.ThroughputMBps, want)
	}
	if r.LatencyP99Us != 2310 { // 2310000 ns → µs
		t.Errorf("p99=%v want 2310", r.LatencyP99Us)
	}
	if r.LatencyP50Us != 98 {
		t.Errorf("p50=%v want 98", r.LatencyP50Us)
	}
	if r.TotalOps != 25600 || r.Errors != 0 {
		t.Errorf("ops=%d err=%d", r.TotalOps, r.Errors)
	}
}

// mixed workload: latency percentiles come from the busier (write) direction.
const fioMixedJSON = `{"jobs":[{"jobname":"mixed-rw","error":0,"job_runtime":1000,
  "read":{"io_bytes":100,"bw_bytes":100,"iops":10,"total_ios":10,
    "clat_ns":{"mean":1000,"percentile":{"99.000000":1000}}},
  "write":{"io_bytes":900,"bw_bytes":900,"iops":90,"total_ios":90,
    "clat_ns":{"mean":5000,"percentile":{"99.000000":9000}}}}]}`

func TestParseFioJSON_MixedUsesBusierSide(t *testing.T) {
	r, err := parseFioJSON([]byte(fioMixedJSON))
	if err != nil {
		t.Fatal(err)
	}
	if r.IOPS != 100 { // 10 read + 90 write
		t.Errorf("iops=%v want 100", r.IOPS)
	}
	if r.LatencyP99Us != 9 { // write side wins (more ops)
		t.Errorf("p99=%v want 9 (write side)", r.LatencyP99Us)
	}
}

func TestParseFioJSON_SkipsNotePrefix(t *testing.T) {
	noisy := "note: queue depth will be capped at 1\nnote: another line\n" + fioRandReadJSON
	r, err := parseFioJSON([]byte(noisy))
	if err != nil {
		t.Fatal(err)
	}
	if r.IOPS != 8533.2 {
		t.Errorf("iops=%v want 8533.2 (note prefix not skipped)", r.IOPS)
	}
}

func TestParseFioJSON_NoJobs(t *testing.T) {
	if _, err := parseFioJSON([]byte(`{"jobs":[]}`)); err == nil {
		t.Fatal("want error on empty jobs")
	}
}

func TestExpandJob_DefaultsAndOverrides(t *testing.T) {
	tmpl := "ioengine=${FIO_ENGINE:-libaio}\nnumjobs=${BENCH_THREADS:-4}\nx=${UNSET}"
	got := ExpandJob(tmpl, map[string]string{"FIO_ENGINE": "psync"})
	if !strings.Contains(got, "ioengine=psync") {
		t.Errorf("override not applied: %q", got)
	}
	if !strings.Contains(got, "numjobs=4") {
		t.Errorf("default not applied: %q", got)
	}
	if !strings.Contains(got, "x=\n") && !strings.HasSuffix(got, "x=") {
		t.Errorf("unset var should be empty: %q", got)
	}
}

func TestLoadJob_AllWorkloadsEmbedded(t *testing.T) {
	for _, w := range KnownWorkloads {
		body, err := loadJob(w, jobDefaults("psync", "/mnt/bench", "64k", 4, 60))
		if err != nil {
			t.Errorf("%s: %v", w, err)
		}
		if !strings.Contains(body, "[") { // fio job header
			t.Errorf("%s: not a fio job: %q", w, body)
		}
		if strings.Contains(body, "${") {
			t.Errorf("%s: unexpanded var remains: %q", w, body)
		}
	}
}

func TestResume_SkipsExistingResult(t *testing.T) {
	dir := t.TempDir()
	c := CellResult{System: "local", Workload: "seq-read", Size: "small", Protocol: "local", Pass: "warm"}
	if ResultExists(dir, c.Slug()) {
		t.Fatal("should not exist yet")
	}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	if !ResultExists(dir, c.Slug()) {
		t.Fatal("should exist after save")
	}
	// .tmp partials must not count as completed cells.
	rs, err := LoadResults(dir)
	if err != nil || len(rs) != 1 {
		t.Fatalf("loadResults: %v n=%d", err, len(rs))
	}
	if filepath.Base(rs[0].System) != "local" {
		t.Errorf("bad load: %+v", rs[0])
	}
}
