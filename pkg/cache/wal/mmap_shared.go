// mmap_shared.go contains constants and types shared between
// mmap.go (Unix) and mmap_windows.go (Windows).

package wal

// mmap file constants
const (
	mmapMagic        = "DTTC"           // DittoFS Cache
	mmapVersion      = uint16(2)        // Version 2 for block-level format
	mmapHeaderSize   = 64               // Header size in bytes
	mmapInitialSize  = 64 * 1024 * 1024 // 64MB initial file size
	mmapGrowthFactor = 2                // Double size when growing
)

// Entry types for the append-only log
const (
	entryTypeBlockWrite    uint8 = 0
	entryTypeBlockUploaded uint8 = 1 // Marks block as uploaded to S3
	entryTypeRemove        uint8 = 3
)

// Header field offsets
const (
	headerOffsetMagic         = 0
	headerOffsetVersion       = 4
	headerOffsetEntryCount    = 6
	headerOffsetNextOffset    = 10
	headerOffsetTotalDataSize = 18
)

// mmapHeader represents the header of the mmap file.
type mmapHeader struct {
	Magic         [4]byte
	Version       uint16
	EntryCount    uint32
	NextOffset    uint64
	TotalDataSize uint64
}
