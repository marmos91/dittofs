// Package format encodes and decodes the journal's on-disk structures, field
// by field from the format's byte tables: packed, little-endian, every
// checksum last. A decode returns a structure, or an error matching one
// outcome class; the caller decides what a class does to the open.
package format
