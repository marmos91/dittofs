// Package index is the journal's placement index: for each file, which
// records hold which byte ranges of it. It is pure in-memory state, rebuilt
// from records at open and never stored, and it does no I/O.
package index
