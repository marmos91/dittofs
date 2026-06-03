// Package orchestrator runs a manifest of DittoFS benchmark workloads and
// emits a versioned, machine-readable result document so performance can be
// tracked over time and gated in CI.
//
// It is an orchestration layer on top of the existing bench/blockstore
// primitives — it does not implement workloads itself. A WorkloadRunner is
// injected (cmd/bench wires the real engine-backed runner; tests inject a
// fast fake), which keeps this package free of pprof/engine/runtime deps and
// trivially testable.
//
// The emitted document is versioned via SchemaVersion. See the "Version
// contract" comment on that constant for how consumers detect and handle
// version bumps.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// SchemaVersion is the version of the Document JSON schema. It starts at 1 and
// is bumped on every breaking change to the document shape.
//
// Version contract:
//
//   - The top-level "schema_version" field is the FIRST and ONLY field a
//     consumer must read before interpreting anything else. It is an integer
//     and is always present.
//   - MINOR, backward-compatible additions (new optional fields) do NOT bump
//     SchemaVersion. Consumers MUST ignore unknown fields (encoding/json does
//     this by default) so they keep working across additive changes.
//   - A bump signals a BREAKING change (a field removed, renamed, or its
//     meaning/units changed). A consumer reading a document whose
//     schema_version it does not recognize MUST refuse to interpret the
//     per-workload numbers rather than silently mis-reading them. Use
//     CheckVersion / DecodeDocument to enforce this.
//   - Producers always stamp the current SchemaVersion via NewDocument.
const SchemaVersion = 1

// Outcome is the run/workload status. The set is closed and mirrors the
// versioned-result-schema contract: a CI gate asserts Outcome == OutcomeCompleted
// across the run and every workload.
type Outcome string

const (
	// OutcomeCompleted means every workload ran to completion with no errors.
	OutcomeCompleted Outcome = "completed"
	// OutcomePartial means at least one workload failed but others completed;
	// the document still carries the results that did run.
	OutcomePartial Outcome = "partial"
	// OutcomeAborted means the run was halted before finishing the manifest
	// (e.g. a fatal setup error). AbortReason carries the cause.
	OutcomeAborted Outcome = "aborted"
)

// Document is the top-level versioned result. SchemaVersion is first so a
// consumer can branch on it before reading anything else.
type Document struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	// Timestamp is the run start time in RFC3339 (UTC). It is injected by the
	// caller (flag or build info), never read from time.Now() inside the run
	// loop, so runs are reproducible and tests are deterministic.
	Timestamp string `json:"timestamp"`
	GitSHA    string `json:"git_sha"`
	System    System `json:"system"`

	Outcome     Outcome `json:"outcome"`
	AbortReason string  `json:"abort_reason,omitempty"`

	// Workloads is keyed by workload entry name (manifest is name-unique).
	Workloads map[string]WorkloadResult `json:"workloads"`
}

// System captures the host/runtime environment the run executed on so results
// from different machines are not silently compared.
type System struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	NumCPU    int    `json:"num_cpu"`
	GoVersion string `json:"go_version"`
	Hostname  string `json:"hostname,omitempty"`
}

// WorkloadResult is the per-workload outcome plus the measured numbers and the
// pprof profile files captured for it (paths relative to the run, or absolute —
// the producer decides; consumers treat them as opaque locators).
type WorkloadResult struct {
	Outcome Outcome `json:"outcome"`
	// Error is the failure message when Outcome != completed; empty otherwise.
	Error string `json:"error,omitempty"`

	// Params echoes the manifest entry that produced this result so a document
	// is self-describing without the manifest.
	Params WorkloadParams `json:"params"`

	// Metrics is omitted when the workload did not complete.
	Metrics *Metrics `json:"metrics,omitempty"`

	// ProfilePaths are the pprof files captured for this workload (cpu, heap,
	// goroutine, and — when full profiling is on — mutex/block). May be empty
	// when profiling is disabled (e.g. under test).
	ProfilePaths []string `json:"profile_paths,omitempty"`
}

// Metrics holds the measured numbers for one workload run.
//
// Latency, OpCounts, and Errors are ADDITIVE fields (introduced after
// schema_version 1). Per the version contract they do NOT bump SchemaVersion:
// they are omitempty so older documents that lack them round-trip unchanged,
// and consumers that predate them ignore the unknown fields. A producer that
// records per-op timings populates them; one that does not leaves them nil/zero.
type Metrics struct {
	DurationNs int64   `json:"duration_ns"`
	Ops        int64   `json:"ops"`
	NsPerOp    float64 `json:"ns_per_op"`
	OpsPerSec  float64 `json:"ops_per_sec"`
	// Bytes is total bytes moved; BytesPerSec is throughput. Both 0 for
	// workloads that do not move payload (walk/delete/gc).
	Bytes       int64   `json:"bytes"`
	BytesPerSec float64 `json:"bytes_per_sec"`

	// Latency carries the per-op p50/p95/p99 distribution. Nil when the runner
	// did not record per-op timings.
	Latency *Latency `json:"latency,omitempty"`
	// OpCounts breaks Ops into succeeded/failed. Zero value when not recorded.
	OpCounts OpCounts `json:"ops_breakdown,omitempty"`
	// Errors is the structured per-op failure tally. Empty when no op failed (or
	// when failures were not recorded).
	Errors []OpError `json:"errors,omitempty"`
}

// Latency is the per-op latency distribution for one workload, in nanoseconds.
type Latency struct {
	P50Ns int64 `json:"p50_ns"`
	P95Ns int64 `json:"p95_ns"`
	P99Ns int64 `json:"p99_ns"`
}

// OpCounts splits a workload's operations into succeeded and failed. Total is
// the sum and mirrors Metrics.Ops; it is stored so a consumer reading only the
// breakdown is self-describing.
type OpCounts struct {
	Total     int64 `json:"total"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
}

// OpError is a structured failure record: the op kind, the offset it targeted
// (0 when not applicable), a coarse error kind, and how many times it occurred.
// It mirrors the PLAN target schema's errors:[{op,offset,error_kind,count}].
type OpError struct {
	Op        string `json:"op"`
	Offset    int64  `json:"offset,omitempty"`
	ErrorKind string `json:"error_kind"`
	Count     int64  `json:"count"`
}

// NewDocument builds a Document stamped with the current SchemaVersion and the
// injected run metadata. Workloads starts empty and is filled by the run loop.
func NewDocument(runID, timestamp, gitSHA string, sys System) *Document {
	return &Document{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Timestamp:     timestamp,
		GitSHA:        gitSHA,
		System:        sys,
		Outcome:       OutcomeCompleted,
		Workloads:     map[string]WorkloadResult{},
	}
}

// Marshal renders the document as indented JSON with a trailing newline.
func (d *Document) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal result document: %w", err)
	}
	return append(b, '\n'), nil
}

// CheckVersion enforces the version contract: it accepts a document whose
// schema_version equals the SchemaVersion this binary was built with, and
// rejects anything else with an actionable error. Callers that want to read an
// older document must add explicit migration code — silent reinterpretation is
// the failure mode this guards against.
func CheckVersion(got int) error {
	if got != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (this build understands %d): "+
			"refusing to interpret results — upgrade the tool or migrate the document", got, SchemaVersion)
	}
	return nil
}

// DecodeDocument reads a result document from r and verifies its schema_version
// before returning it. It is the safe entry point for consumers (compare mode,
// CI gates) — it never hands back a document it cannot correctly interpret.
func DecodeDocument(r io.Reader) (*Document, error) {
	var d Document
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode result document: %w", err)
	}
	if err := CheckVersion(d.SchemaVersion); err != nil {
		return nil, err
	}
	return &d, nil
}

// DecodeFile opens path and decodes a result document from it via
// DecodeDocument (so the schema_version is verified). It is the file-path
// convenience used by compare mode and the remote orchestrator.
func DecodeFile(path string) (*Document, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied
	if err != nil {
		return nil, fmt.Errorf("open result %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return DecodeDocument(f)
}

// ParseTimestamp validates an RFC3339 timestamp string, returning it normalized
// to UTC RFC3339. An empty string is rejected — the caller must supply one
// (flag or build info) so documents are never stamped with a hidden time.Now().
func ParseTimestamp(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("timestamp is required (pass --timestamp RFC3339)")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp %q (want RFC3339, e.g. 2026-01-02T15:04:05Z): %w", s, err)
	}
	return t.UTC().Format(time.RFC3339), nil
}
