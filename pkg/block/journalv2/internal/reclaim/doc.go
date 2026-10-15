// Package reclaim returns the journal's storage to the filesystem. Repack
// copies the still-live records of a segment into another segment, repoints
// the placement index and removes the original; a segment left with nothing
// live is retired. Reclamation is local and discards no live content.
package reclaim
