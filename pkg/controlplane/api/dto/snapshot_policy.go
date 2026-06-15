package dto

import "time"

// SnapshotPolicy is the wire representation of a per-share snapshot policy.
// Interval and TTL are rendered as Go duration strings (e.g. "24h0m0s") so the
// surface stays human-readable; a zero TTL is omitted.
type SnapshotPolicy struct {
	Share      string     `json:"share"`
	Enabled    bool       `json:"enabled"`
	Interval   string     `json:"interval"`
	KeepLast   int        `json:"keep_last"`
	TTL        string     `json:"ttl,omitempty"`
	NamePrefix string     `json:"name_prefix,omitempty"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// UpsertSnapshotPolicyRequest is the body for PUT
// .../shares/{name}/snapshot-policy. Interval accepts a Go duration or an
// @-shorthand (@hourly/@daily/@weekly). TTL accepts a Go duration ("" or "0"
// disables the age bound). KeepLast 0 disables the count bound. Enabled
// defaults to true when omitted.
type UpsertSnapshotPolicyRequest struct {
	Interval   string `json:"interval"`
	KeepLast   int    `json:"keep_last,omitempty"`
	TTL        string `json:"ttl,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
	NamePrefix string `json:"name_prefix,omitempty"`
}
