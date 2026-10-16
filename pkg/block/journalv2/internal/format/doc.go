// Package format encodes and decodes the journal's on-disk structures: the
// format file, segment headers, records, segment catalogs and trailers. Each
// structure carries a CRC32C checksum, and a structure whose checksum does not
// verify is rejected rather than repaired.
//
// The package performs no I/O: it converts between bytes and values.
package format
