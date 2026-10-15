// Package index is the journal's placement index, kept in memory only. For
// each file it holds a set of disjoint held extents, each mapped to the
// location of the record that stores it and carrying its content version and
// offloaded bit, together with the removal markers not yet settled. Where two
// records cover the same byte, the newest version wins.
//
// The index is the only structure consulted to serve a read. It performs no
// I/O; at open it is rebuilt from what is on disk.
package index
