package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// maxManifestCheckFindings bounds how many damaged payloads are reported in
// detail. The aggregate counters stay exact past the cap; only the per-payload
// list stops growing, so a store where every file is damaged still produces a
// bounded response.
const maxManifestCheckFindings = 1000

// maxManifestCheckRangesPerPayload bounds the per-payload range and row lists
// for the same reason: one heavily fragmented file must not be able to fill the
// whole report on its own.
const maxManifestCheckRangesPerPayload = 32

// ByteRange is a half-open [Start, End) span of a payload that no manifest row
// covers.
type ByteRange struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`

	// Claimed reports whether the file's own block list (FileAttr.Blocks)
	// says data lives in this span. A claimed span with no covering row is
	// damage: the file names a chunk the manifest cannot place. An unclaimed
	// span is indistinguishable from a legitimate sparse hole or from bytes
	// written but not yet rolled up into the manifest, so it is reported and
	// not counted as damage.
	Claimed bool `json:"claimed"`
}

// PayloadFinding is the per-payload detail for one file the scan found
// something to say about.
type PayloadFinding struct {
	// Path is the file's path within the share, for operators who need to
	// act on the result rather than only count it.
	Path string `json:"path"`

	// PayloadID is the content identifier whose manifest was scanned.
	PayloadID string `json:"payload_id"`

	// Size is the file's recorded size — the extent the manifest is
	// expected to cover.
	Size uint64 `json:"size"`

	// Uncovered lists the spans of [0, Size) that no placeable manifest row
	// with committed bytes covers.
	Uncovered []ByteRange `json:"uncovered,omitempty"`

	// UnplaceableRows lists the IDs of manifest rows that exist but carry no
	// parseable chunk offset, so no reader can say which bytes they hold.
	UnplaceableRows []string `json:"unplaceable_rows,omitempty"`

	// UnknownHashRows lists the IDs of manifest rows whose content hash the
	// synced-hash store has no record of, i.e. rows claiming bytes that were
	// never confirmed mirrored to the remote.
	UnknownHashRows []string `json:"unknown_hash_rows,omitempty"`

	// Truncated reports that at least one of the lists above stopped short
	// of the payload's full set.
	Truncated bool `json:"truncated,omitempty"`
}

// Damaged reports whether this payload holds evidence of manifest damage, as
// opposed to holes the scan cannot tell apart from sparseness.
func (p *PayloadFinding) Damaged() bool {
	if len(p.UnplaceableRows) > 0 || len(p.UnknownHashRows) > 0 {
		return true
	}
	for _, r := range p.Uncovered {
		if r.Claimed {
			return true
		}
	}
	return false
}

// ManifestCheckResult is the outcome of a manifest-coverage scan over one
// share. Every count is exact; only the Findings list is capped.
type ManifestCheckResult struct {
	// Share is the share whose metadata store was scanned.
	Share string `json:"share"`

	// StartedAt is when the scan began (UTC).
	StartedAt time.Time `json:"started_at"`

	// CompletedAt is when the scan finished (UTC).
	CompletedAt time.Time `json:"completed_at"`

	// DurationMS is the wall-clock cost in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// FilesScanned is the number of regular files walked.
	FilesScanned uint64 `json:"files_scanned"`

	// SyncedHashesChecked reports whether the unknown-hash check ran. It is
	// skipped on a share with no remote store, where nothing is ever marked
	// synced and every row would otherwise be reported.
	SyncedHashesChecked bool `json:"synced_hashes_checked"`

	// PayloadsWithFindings is the number of payloads the scan had something
	// to report about, damage or not.
	PayloadsWithFindings uint64 `json:"payloads_with_findings"`

	// DamagedPayloads is the number of payloads holding evidence of damage:
	// a claimed-but-uncovered span, an unplaceable row, or an unknown hash.
	DamagedPayloads uint64 `json:"damaged_payloads"`

	// UncoveredRanges and UncoveredBytes total every uncovered span,
	// claimed or not.
	UncoveredRanges uint64 `json:"uncovered_ranges"`
	UncoveredBytes  uint64 `json:"uncovered_bytes"`

	// ClaimedUncoveredRanges and ClaimedUncoveredBytes total the subset the
	// files' own block lists claim to hold data — the damaged subset.
	ClaimedUncoveredRanges uint64 `json:"claimed_uncovered_ranges"`
	ClaimedUncoveredBytes  uint64 `json:"claimed_uncovered_bytes"`

	// UnplaceableRows is the total count of manifest rows with no parseable
	// chunk offset.
	UnplaceableRows uint64 `json:"unplaceable_rows"`

	// UnknownHashRows is the total count of manifest rows whose hash the
	// synced-hash store does not know. Always zero when
	// SyncedHashesChecked is false.
	UnknownHashRows uint64 `json:"unknown_hash_rows"`

	// Findings is the per-payload detail, capped at
	// maxManifestCheckFindings entries.
	Findings []PayloadFinding `json:"findings,omitempty"`

	// FindingsTruncated reports that more payloads had findings than the
	// list holds.
	FindingsTruncated bool `json:"findings_truncated,omitempty"`
}

// Damaged reports whether the scan found evidence of manifest damage. Holes
// that no file claims do not count — see ByteRange.Claimed.
func (r *ManifestCheckResult) Damaged() bool {
	return r.ClaimedUncoveredRanges > 0 || r.UnplaceableRows > 0 || r.UnknownHashRows > 0
}

// CheckManifests walks every regular file in a share and compares the byte
// ranges its manifest rows cover against the file's recorded size, reporting
// three structural defects:
//
//  1. spans of the file no manifest row covers,
//  2. rows that exist but carry no parseable chunk offset, so no reader can
//     place them (block.ErrManifestInconsistent at read time),
//  3. rows whose content hash the synced-hash store has no record of.
//
// It is metadata-only. No block is fetched, no local data is read and no
// remote object is touched, so the cost is a metadata walk regardless of how
// much data the share holds — cheap enough to answer "how many files are
// affected" store-wide.
//
// The coverage view is the manifest's alone, which is deliberately narrower
// than DataExtents: that unions the local journal tier as well, so it reports
// data for bytes written but not yet rolled up. Counting those as covered here
// would hide exactly the manifest gap this scan exists to find, at the cost of
// reporting a not-yet-rolled-up range as an uncovered span. Such a span carries
// no claim from the file's own block list, and the report keeps claimed and
// unclaimed spans apart for that reason.
//
// checkSynced gates the third check. A share with no remote store never marks
// a hash synced, so running it there would report every row in the share.
func CheckManifests(ctx context.Context, share string, store metadata.Store, checkSynced bool) (*ManifestCheckResult, error) {
	if store == nil {
		return nil, errors.New("store check: metadata store is nil")
	}
	if share == "" {
		return nil, errors.New("store check: share is empty")
	}

	result := &ManifestCheckResult{
		Share:               share,
		StartedAt:           time.Now().UTC(),
		SyncedHashesChecked: checkSynced,
	}

	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		return nil, fmt.Errorf("store check: get root handle for %q: %w", share, err)
	}
	if err := walkAuditShareFiles(ctx, store, rootHandle, "", func(path string, f *metadata.File) error {
		result.FilesScanned++
		finding, err := checkFileManifest(ctx, store, path, f, checkSynced)
		if err != nil {
			return err
		}
		if finding == nil {
			return nil
		}
		result.accumulate(finding)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("store check: walk share %q: %w", share, err)
	}

	result.CompletedAt = time.Now().UTC()
	result.DurationMS = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	return result, nil
}

// accumulate folds one payload's finding into the running totals and appends
// it to the capped detail list.
func (r *ManifestCheckResult) accumulate(f *PayloadFinding) {
	r.PayloadsWithFindings++
	if f.Damaged() {
		r.DamagedPayloads++
	}
	for _, rng := range f.Uncovered {
		r.UncoveredRanges++
		r.UncoveredBytes += rng.End - rng.Start
		if rng.Claimed {
			r.ClaimedUncoveredRanges++
			r.ClaimedUncoveredBytes += rng.End - rng.Start
		}
	}
	r.UnplaceableRows += uint64(len(f.UnplaceableRows))
	r.UnknownHashRows += uint64(len(f.UnknownHashRows))

	if len(r.Findings) >= maxManifestCheckFindings {
		r.FindingsTruncated = true
		return
	}
	r.Findings = append(r.Findings, *f)
}

// checkFileManifest scans one file's manifest rows. Returns nil when the file
// has nothing to report.
//
// A row with a zero hash holds no committed bytes yet, so it contributes no
// coverage and is not put to the synced-hash store — the same reading
// DataExtents takes of a pending row.
func checkFileManifest(
	ctx context.Context,
	store metadata.Store,
	path string,
	f *metadata.File,
	checkSynced bool,
) (*PayloadFinding, error) {
	payloadID := string(f.PayloadID)
	if payloadID == "" {
		return nil, nil
	}

	rows, err := store.ListFileChunks(ctx, payloadID)
	if err != nil && !errors.Is(err, block.ErrFileChunkNotFound) {
		return nil, fmt.Errorf("list file chunks for payload %q: %w", payloadID, err)
	}

	finding := &PayloadFinding{Path: path, PayloadID: payloadID, Size: f.Size}
	covered := make([][2]uint64, 0, len(rows))

	for _, row := range rows {
		if row == nil {
			continue
		}
		off, ok := block.ParseChunkOffset(row.ID)
		if !ok {
			appendCapped(&finding.UnplaceableRows, row.ID, &finding.Truncated)
			continue
		}
		if row.Hash.IsZero() {
			continue
		}
		if checkSynced {
			synced, serr := store.IsSynced(ctx, row.Hash)
			if serr != nil {
				return nil, fmt.Errorf("is synced %s: %w", row.Hash, serr)
			}
			if !synced {
				appendCapped(&finding.UnknownHashRows, row.ID, &finding.Truncated)
			}
		}
		if e, ok := clipRange(off, uint64(row.DataSize), f.Size); ok {
			covered = append(covered, e)
		}
	}

	claimed := make([][2]uint64, 0, len(f.Blocks))
	for _, ref := range f.Blocks {
		if e, ok := clipRange(ref.Offset, uint64(ref.Size), f.Size); ok {
			claimed = append(claimed, e)
		}
	}

	for _, hole := range uncoveredRanges(coalesceExtents(covered), f.Size) {
		for _, piece := range splitByClaim(hole, coalesceExtents(claimed)) {
			appendCapped(&finding.Uncovered, piece, &finding.Truncated)
		}
	}

	if len(finding.Uncovered) == 0 && len(finding.UnplaceableRows) == 0 && len(finding.UnknownHashRows) == 0 {
		return nil, nil
	}
	return finding, nil
}

// appendCapped appends to a per-payload detail list until it reaches
// maxManifestCheckRangesPerPayload, flagging truncation once it stops.
func appendCapped[T any](dst *[]T, v T, truncated *bool) {
	if len(*dst) >= maxManifestCheckRangesPerPayload {
		*truncated = true
		return
	}
	*dst = append(*dst, v)
}

// clipRange returns [off, off+length) clipped to [0, size), and whether
// anything survived the clip. An overflowing end is clamped to size.
func clipRange(off, length, size uint64) ([2]uint64, bool) {
	if length == 0 || off >= size {
		return [2]uint64{}, false
	}
	end := off + length
	if end < off || end > size {
		end = size
	}
	return [2]uint64{off, end}, true
}

// uncoveredRanges returns [0, size) minus covered, which must already be
// sorted and non-overlapping (coalesceExtents output).
func uncoveredRanges(covered [][2]uint64, size uint64) [][2]uint64 {
	var (
		out [][2]uint64
		cur uint64
	)
	for _, c := range covered {
		if c[0] > cur {
			out = append(out, [2]uint64{cur, c[0]})
		}
		if c[1] > cur {
			cur = c[1]
		}
	}
	if cur < size {
		out = append(out, [2]uint64{cur, size})
	}
	return out
}

// splitByClaim cuts one uncovered span against the file's own block list,
// tagging each piece with whether the file claims data there. claimed must be
// sorted and non-overlapping.
func splitByClaim(hole [2]uint64, claimed [][2]uint64) []ByteRange {
	var out []ByteRange
	cur := hole[0]
	for _, c := range claimed {
		if c[1] <= cur {
			continue
		}
		if c[0] >= hole[1] {
			break
		}
		start := c[0]
		if start < cur {
			start = cur
		}
		end := c[1]
		if end > hole[1] {
			end = hole[1]
		}
		if start > cur {
			out = append(out, ByteRange{Start: cur, End: start})
		}
		out = append(out, ByteRange{Start: start, End: end, Claimed: true})
		cur = end
	}
	if cur < hole[1] {
		out = append(out, ByteRange{Start: cur, End: hole[1]})
	}
	return out
}
