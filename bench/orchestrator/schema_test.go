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

// TestAdditiveLatencyFields confirms the latency / ops-breakdown / errors
// fields round-trip without bumping schema_version. They are additive: a
// document carrying them must still report schema_version 1.
func TestAdditiveLatencyFields(t *testing.T) {
	doc := sampleDoc()
	w := doc.Workloads["seq-write"]
	w.Metrics.Latency = &Latency{P50Ns: 100, P95Ns: 500, P99Ns: 900}
	w.Metrics.OpCounts = &OpCounts{Total: 1000, Succeeded: 998, Failed: 2}
	w.Metrics.Errors = []OpError{{Op: "write", Offset: 4096, ErrorKind: "timeout", Count: 2}}
	doc.Workloads["seq-write"] = w

	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeDocument(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("additive fields must not bump schema_version: got %d, want 1", got.SchemaVersion)
	}
	gw := got.Workloads["seq-write"]
	if gw.Metrics.Latency == nil || gw.Metrics.Latency.P95Ns != 500 {
		t.Errorf("latency not round-tripped: %+v", gw.Metrics.Latency)
	}
	if gw.Metrics.OpCounts == nil || gw.Metrics.OpCounts.Failed != 2 || gw.Metrics.OpCounts.Total != 1000 {
		t.Errorf("ops breakdown not round-tripped: %+v", gw.Metrics.OpCounts)
	}
	if len(gw.Metrics.Errors) != 1 || gw.Metrics.Errors[0].ErrorKind != "timeout" {
		t.Errorf("errors not round-tripped: %+v", gw.Metrics.Errors)
	}
}

// TestOmittedLatencyFields confirms a document WITHOUT the additive fields
// (the schema_version-1 shape) still decodes cleanly and leaves them empty —
// the backward-compatibility half of the additive contract.
func TestOmittedLatencyFields(t *testing.T) {
	doc := sampleDoc() // sampleDoc sets no latency/ops/errors
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The emitted JSON must not carry empty additive objects (omitempty). This
	// is exactly why OpCounts is a pointer — a value struct would always encode.
	for _, field := range []string{`"latency"`, `"ops_breakdown"`, `"errors"`} {
		if bytes.Contains(b, []byte(field)) {
			t.Errorf("%s present despite no samples (omitempty failed):\n%s", field, b)
		}
	}
	got, err := DecodeDocument(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	gw := got.Workloads["seq-write"]
	if gw.Metrics.Latency != nil {
		t.Errorf("latency should be nil, got %+v", gw.Metrics.Latency)
	}
	if gw.Metrics.OpCounts != nil {
		t.Errorf("op counts should be nil, got %+v", gw.Metrics.OpCounts)
	}
}
