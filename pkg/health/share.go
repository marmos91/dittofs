package health

import "time"

// ShareStatus is a share's health report plus the structured per-share
// counters that belong next to it. The [Report] is embedded, so the JSON
// form is a plain report with optional extra objects hanging off it and
// every existing consumer of the report fields is unaffected.
//
// Counters live here rather than in [Report.Message] because they are
// numbers an operator charts and alerts on, and because a share can have
// several independent things worth reporting at once — prose collapses
// them into one string that nothing can parse back apart.
type ShareStatus struct {
	Report

	// Integrity is the outcome of the most recent structural manifest
	// scan for this share. Nil when no scan has run yet.
	Integrity *IntegrityStatus `json:"integrity,omitempty"`
}

// IntegrityStatus summarises one structural manifest scan: what it looked
// at, when, and what it found. It is metadata-only — no block is fetched
// and no remote object is touched to produce it.
//
// DamagedPayloads is the field that matters. A payload is damaged when the
// scan found evidence the manifest disagrees with the file's own block
// list: a claimed-but-uncovered span, a row carrying no placeable chunk
// offset, or a row whose hash the synced-hash store does not know. The
// first of those is invisible at read time — an absent row is how sparse
// holes are represented, so the read path returns zeros and reports
// success — which is why the scan exists.
type IntegrityStatus struct {
	// LastRunAt is when the scan completed (UTC).
	LastRunAt time.Time `json:"last_run_at"`

	// DurationMS is the scan's wall-clock cost in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// FilesScanned is the number of regular files walked.
	FilesScanned uint64 `json:"files_scanned"`

	// PayloadsWithFindings is the number of payloads the scan had
	// something to say about, damage or not.
	PayloadsWithFindings uint64 `json:"payloads_with_findings"`

	// DamagedPayloads is the subset of those holding evidence of damage.
	DamagedPayloads uint64 `json:"damaged_payloads"`

	// Findings by kind, so an operator can tell which defect is present
	// without re-running the scan by hand.
	ClaimedUncoveredRanges uint64 `json:"claimed_uncovered_ranges"`
	UnplaceableRows        uint64 `json:"unplaceable_rows"`
	UnknownHashRows        uint64 `json:"unknown_hash_rows"`

	// Error names why the scan did not complete, empty when it did. A
	// failed scan keeps the previous run's counters out of the report
	// rather than presenting stale numbers as current.
	Error string `json:"error,omitempty"`
}
