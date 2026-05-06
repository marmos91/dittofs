package engine

import "errors"

// ErrLegacyReadOnCASOnly is returned by the dual-read shim when it would
// otherwise fall back to the legacy `{payloadID}/block-{idx}` read path
// on a share whose BlockLayout has been flipped to cas-only (Plan 14-02 /
// MIG-03 / D-A8). Surfacing this is a fail-loud signal:
//
//   - The share's metadata still contains stale legacy FileBlock rows
//     (a migration bug — `dfsctl blockstore migrate` left work behind),
//     OR
//   - A write is racing the cutover (Plan 14-05 ensures the cutover is
//     offline-only, so this should be structurally impossible in
//     production), OR
//   - An operator hand-edited the share's `block_layout` column without
//     running the migration first.
//
// Operator action: re-run `dfsctl blockstore migrate --share <name>`,
// or roll the BlockLayout back to legacy via direct DB intervention
// while the offending share is offline. Returning silent zeros (the
// pre-Phase-14 behavior on a missing legacy key) would mask post-
// migration drift, so the gate refuses the read instead.
var ErrLegacyReadOnCASOnly = errors.New("legacy read attempted on cas-only share (MIG-03)")
