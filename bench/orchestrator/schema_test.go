package orchestrator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleDoc() *Document {
	doc := NewDocument("run-1", "2026-01-02T15:04:05Z", "abc123", System{
		OS: "linux", Arch: "arm64", NumCPU: 8, GoVersion: "go1.24",
	})
	m := MetricsFromRun(2_000_000_000, 1000, 8_000_000)
	doc.Workloads["seq-write"] = WorkloadResult{
		Outcome:      OutcomeCompleted,
		Params:       WorkloadParams{Name: "seq-write", Workload: "sequential-write", Ops: 1000, Seed: 1},
		Metrics:      &m,
		ProfilePaths: []string{"p/cpu.pprof"},
	}
	return doc
}

func TestDocumentRoundTrip(t *testing.T) {
	doc := sampleDoc()
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := DecodeDocument(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.RunID != "run-1" || got.GitSHA != "abc123" {
		t.Errorf("metadata not round-tripped: %+v", got)
	}
	w, ok := got.Workloads["seq-write"]
	if !ok {
		t.Fatal("seq-write workload missing after round-trip")
	}
	if w.Metrics == nil || w.Metrics.Ops != 1000 {
		t.Errorf("metrics not round-tripped: %+v", w.Metrics)
	}
	if w.Metrics.NsPerOp != 2_000_000 {
		t.Errorf("ns_per_op = %v, want 2000000", w.Metrics.NsPerOp)
	}
}

func TestSchemaVersionAlwaysPresent(t *testing.T) {
	b, err := sampleDoc().Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["schema_version"]; !ok {
		t.Fatal("schema_version field absent from emitted JSON")
	}
}

func TestVersionMismatchRejected(t *testing.T) {
	// A document stamped with a future schema_version must be refused so a
	// consumer never silently mis-reads renamed/reinterpreted fields.
	doc := sampleDoc()
	doc.SchemaVersion = SchemaVersion + 1
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := DecodeDocument(bytes.NewReader(b)); err == nil {
		t.Fatal("expected version-mismatch error, got nil")
	}

	if err := CheckVersion(SchemaVersion); err != nil {
		t.Errorf("CheckVersion(current) = %v, want nil", err)
	}
	if err := CheckVersion(0); err == nil {
		t.Error("CheckVersion(0) = nil, want error")
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	// Additive (minor) schema changes must not break older consumers: an
	// extra field is silently ignored.
	in := `{"schema_version":1,"run_id":"r","future_field":42,"workloads":{}}`
	got, err := DecodeDocument(strings.NewReader(in))
	if err != nil {
		t.Fatalf("decode with unknown field: %v", err)
	}
	if got.RunID != "r" {
		t.Errorf("run_id = %q, want r", got.RunID)
	}
}

func TestParseTimestamp(t *testing.T) {
	got, err := ParseTimestamp("2026-01-02T15:04:05Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != "2026-01-02T15:04:05Z" {
		t.Errorf("got %q", got)
	}
	if _, err := ParseTimestamp(""); err == nil {
		t.Error("empty timestamp accepted")
	}
	if _, err := ParseTimestamp("not-a-time"); err == nil {
		t.Error("garbage timestamp accepted")
	}
	// Non-UTC input is normalized to UTC.
	utc, err := ParseTimestamp("2026-01-02T15:04:05+02:00")
	if err != nil {
		t.Fatalf("parse offset: %v", err)
	}
	if utc != "2026-01-02T13:04:05Z" {
		t.Errorf("UTC normalization failed: %q", utc)
	}
}

func TestMetricsFromRunZeroDuration(t *testing.T) {
	m := MetricsFromRun(0, 100, 200)
	if m.OpsPerSec != 0 || m.BytesPerSec != 0 || m.NsPerOp != 0 {
		t.Errorf("zero duration must not divide: %+v", m)
	}
}
