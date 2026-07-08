// Package workloads exposes the fio job files as an embedded filesystem so the
// dfsbench harness (cmd/bench) can materialize them on any target without the
// files needing to live next to the binary. The .fio files remain the single
// source of truth for access patterns; embed.go only makes them reachable.
package workloads

import "embed"

// FS holds every fio job file in this directory.
//
//go:embed *.fio
var FS embed.FS
