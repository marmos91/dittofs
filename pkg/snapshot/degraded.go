package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// DegradedMarkerName is the file written into a snapshot's directory when the
// snapshot could not capture every metadata row. Its presence is what makes a
// degraded snapshot legible as one to anything that reads the directory — the
// dump and the manifest look ordinary, because what is wrong with them is what
// is missing.
//
// Absence means complete: every writer that produces a degraded snapshot must
// write this file before the snapshot leaves state 'creating'.
const DegradedMarkerName = "degraded.json"

// Degraded is the marker's content. Entries is the authoritative count; Keys is
// a bounded sample, enough to go and look at the offending rows.
type Degraded struct {
	// Reason names the class of gap, e.g. "undecodable metadata entries".
	Reason string `json:"reason"`
	// Entries is how many rows the snapshot could not capture.
	Entries int `json:"entries"`
	// Keys samples the store keys of those rows.
	Keys []string `json:"keys,omitempty"`
	// KeysTruncated reports that Keys holds fewer names than Entries.
	KeysTruncated bool `json:"keys_truncated,omitempty"`
}

// WriteDegradedMarker writes d to path via temp + fsync + rename, so a reader
// never sees a half-written marker and, more importantly, never sees the
// snapshot directory without it once the snapshot is complete.
func WriteDegradedMarker(path string, d *Degraded) error {
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: marshal degraded marker: %w", err)
	}
	body = append(body, '\n')

	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot: open temp degraded marker: %w", err)
	}
	_, writeErr := f.Write(body)
	if writeErr == nil {
		if serr := f.Sync(); serr != nil {
			writeErr = fmt.Errorf("snapshot: fsync temp degraded marker: %w", serr)
		}
	}
	if cerr := f.Close(); cerr != nil && writeErr == nil {
		writeErr = fmt.Errorf("snapshot: close temp degraded marker: %w", cerr)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return writeErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("snapshot: rename temp degraded marker: %w", err)
	}
	return nil
}

// ReadDegradedMarker returns the marker at path, or (nil, nil) when there is
// none — the ordinary case, meaning the snapshot is complete. A marker that
// exists but will not parse is an error: a reader must not downgrade "this
// snapshot says something about itself that I cannot read" to "this snapshot
// is fine".
func ReadDegradedMarker(path string) (*Degraded, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot: read degraded marker: %w", err)
	}
	var d Degraded
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("snapshot: parse degraded marker %s: %w", path, err)
	}
	return &d, nil
}
