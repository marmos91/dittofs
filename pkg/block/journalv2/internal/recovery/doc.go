// Package recovery rebuilds the journal's state when it is opened, from the
// segment catalogs or, where a catalog cannot be used, by scanning records. It
// consults nothing outside the journal and does not require the previous
// process to have exited cleanly.
//
// It tells a torn tail from corruption: a torn tail is treated as never
// written and cut off, while a corrupt record stops being held and is
// reported. A segment that belongs to another journal is left alone.
package recovery
