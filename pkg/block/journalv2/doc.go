// Package journalv2 holds the bytes of files on this machine, durably against
// process and machine failure, and answers one question about them: does it
// hold these bytes, at what offset, and which version of them.
//
// There is one journal per device. It holds the files of every share placed
// on that device, and each share works through its own handle. A file's share
// is given by the handle it is written through, never by its identifier.
//
// The journal's unit is an extent: a byte range of one file. It knows nothing
// about the structure of that content: a record is whatever one write staged,
// not a chunk or a block, and the journal knows no remote tier.
//
// A read of bytes the journal does not hold reports exactly which ranges are
// missing and leaves the caller's buffer untouched. It never returns zeros for
// them, and it does not say why they are missing: an extent never written, one
// released by eviction and one whose bytes were lost are reported the same way.
//
// Storage is freed in whole segments only. The caller drives reclamation: it
// offers extents for offload, releases what was offloaded and repacks
// segments. The journal provides the mechanisms and none of the policy.
//
// The package depends only on the standard library and the operating system's
// file interface. Time is read only through an injected clock, so a test can
// advance it.
package journalv2
