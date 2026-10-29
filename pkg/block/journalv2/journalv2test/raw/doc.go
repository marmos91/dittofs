// Package raw writes the journal's on-disk structures by hand, field by field
// from the format's tables, with the standard library's CRC32C.
//
// It shares no code with the journal: a test plants or damages bytes built
// here, and the journal's encoder can be compared with them, so raw stays an
// independent account of what the bytes must be.
//
// Every integer is little-endian and every structure is packed without
// padding. A checksum seeded with the journal identity is the CRC32C of the 16
// identity bytes followed by the bytes it covers.
package raw
