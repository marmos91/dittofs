// mmap.go provides memory-mapped file backing for WAL persistence.
//
// When mmap backing is enabled, cache data is persisted to disk and can
// survive process restarts. The OS handles flushing dirty pages asynchronously,
// so write performance remains similar to pure in-memory operation.
//
// File Format:
// The mmap file uses an append-only log format for crash safety:
//
//	Header (64 bytes):
//	  - Magic: "DTTC" (4 bytes)
//	  - Version: uint16 (2 bytes)
//	  - Entry count: uint32 (4 bytes)
//	  - Next write offset: uint64 (8 bytes)
//	  - Total data size: uint64 (8 bytes)
//	  - Reserved: 38 bytes
//
//	Entries (variable):
//	  - Entry type: uint8 (1 byte) - 0=slice, 1=delete, 2=truncate, 3=remove
//	  - File handle length: uint16 (2 bytes)
//	  - File handle: variable
//	  - Chunk index: uint32 (4 bytes)
//	  - Slice ID: 36 bytes (UUID string)
//	  - Offset in chunk: uint32 (4 bytes)
//	  - Data length: uint32 (4 bytes)
//	  - State: uint8 (1 byte)
//	  - CreatedAt: int64 (8 bytes)
//	  - BlockRef count: uint16 (2 bytes)
//	  - BlockRefs: variable (ID length + ID + size per ref)
//	  - Data: variable
//
// Recovery:
// On startup, the log is replayed to return all slice entries.

package wal

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// mmap file constants
const (
	mmapMagic        = "DTTC" // DittoFS Cache
	mmapVersion      = uint16(1)
	mmapHeaderSize   = 64
	mmapInitialSize  = 64 * 1024 * 1024 // 64MB initial file size
	mmapGrowthFactor = 2                // Double size when growing
)

// Entry types for the append-only log
const (
	entryTypeSlice  uint8 = 0
	entryTypeRemove uint8 = 3
)

// Header field offsets
const (
	headerOffsetMagic         = 0
	headerOffsetVersion       = 4
	headerOffsetEntryCount    = 6
	headerOffsetNextOffset    = 10
	headerOffsetTotalDataSize = 18
)

// mmapHeader represents the header of the mmap file
type mmapHeader struct {
	Magic         [4]byte
	Version       uint16
	EntryCount    uint32
	NextOffset    uint64
	TotalDataSize uint64
}

// MmapPersister implements the Persister interface using memory-mapped files.
type MmapPersister struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	data   []byte // mmap'd region
	size   uint64 // current file/mmap size
	header *mmapHeader
	dirty  bool
	closed bool
}

// NewMmapPersister creates a new mmap-backed persister.
//
// The persister data is stored in an append-only log file at the given path.
// If the file exists, it is opened and validated (but not recovered - call Recover() for that).
// If the file doesn't exist, it is created with initial size.
//
// Parameters:
//   - path: Directory path for the cache file (cache.dat will be created)
func NewMmapPersister(path string) (*MmapPersister, error) {
	// Ensure directory exists
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	p := &MmapPersister{
		path: path,
	}

	// Initialize mmap file
	if err := p.init(); err != nil {
		return nil, fmt.Errorf("init mmap: %w", err)
	}

	return p, nil
}

// init initializes the mmap file, creating it if needed.
func (p *MmapPersister) init() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	filePath := filepath.Join(p.path, "cache.dat")

	// Check if file exists
	_, err := os.Stat(filePath)
	fileExists := err == nil

	if fileExists {
		return p.openExisting(filePath)
	}

	return p.createNew(filePath)
}

// createNew creates a new mmap file with initial size.
func (p *MmapPersister) createNew(filePath string) error {
	// Create file
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	// Extend to initial size
	if err := f.Truncate(int64(mmapInitialSize)); err != nil {
		f.Close()
		return fmt.Errorf("truncate file: %w", err)
	}

	// Memory map
	data, err := unix.Mmap(int(f.Fd()), 0, mmapInitialSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return fmt.Errorf("mmap: %w", err)
	}

	p.file = f
	p.data = data
	p.size = mmapInitialSize

	// Write header
	p.header = &mmapHeader{
		Version:       mmapVersion,
		EntryCount:    0,
		NextOffset:    mmapHeaderSize,
		TotalDataSize: 0,
	}
	copy(p.header.Magic[:], mmapMagic)

	p.writeHeader()

	return nil
}

// openExisting opens an existing mmap file and validates it.
func (p *MmapPersister) openExisting(filePath string) error {
	// Open existing file
	f, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

	// Get file size
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat file: %w", err)
	}

	size := uint64(info.Size())
	if size < mmapHeaderSize {
		f.Close()
		return ErrCorrupted
	}

	// Memory map
	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return fmt.Errorf("mmap: %w", err)
	}

	p.file = f
	p.data = data
	p.size = size

	// Read and validate header
	header := &mmapHeader{}
	copy(header.Magic[:], data[headerOffsetMagic:headerOffsetVersion])
	header.Version = binary.LittleEndian.Uint16(data[headerOffsetVersion:headerOffsetEntryCount])
	header.EntryCount = binary.LittleEndian.Uint32(data[headerOffsetEntryCount:headerOffsetNextOffset])
	header.NextOffset = binary.LittleEndian.Uint64(data[headerOffsetNextOffset:headerOffsetTotalDataSize])
	header.TotalDataSize = binary.LittleEndian.Uint64(data[headerOffsetTotalDataSize:])

	if string(header.Magic[:]) != mmapMagic {
		p.closeLocked()
		return ErrCorrupted
	}

	if header.Version != mmapVersion {
		p.closeLocked()
		return ErrVersionMismatch
	}

	p.header = header

	return nil
}

// AppendSlice appends a slice entry to the WAL.
func (p *MmapPersister) AppendSlice(entry *SliceEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	// Calculate entry size
	entrySize := 1 + // entry type
		2 + len(entry.PayloadID) + // payload ID
		4 + // chunk index
		36 + // slice ID
		4 + // offset
		4 + // length
		1 + // state
		8 + // created at
		2 // block ref count

	for _, ref := range entry.BlockRefs {
		entrySize += 1 + len(ref.ID) + 4 // ID length + ID + Size
	}
	entrySize += len(entry.Data)

	// Ensure space
	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	// Write entry type
	p.data[offset] = entryTypeSlice
	offset++

	// Write payload ID
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(entry.PayloadID)))
	offset += 2
	copy(p.data[offset:], []byte(entry.PayloadID))
	offset += uint64(len(entry.PayloadID))

	// Write chunk index
	binary.LittleEndian.PutUint32(p.data[offset:], entry.ChunkIdx)
	offset += 4

	// Write slice ID (pad to 36 bytes)
	idBytes := []byte(entry.ID)
	if len(idBytes) > 36 {
		idBytes = idBytes[:36]
	}
	copy(p.data[offset:offset+36], idBytes)
	offset += 36

	// Write offset
	binary.LittleEndian.PutUint32(p.data[offset:], entry.Offset)
	offset += 4

	// Write length
	binary.LittleEndian.PutUint32(p.data[offset:], entry.Length)
	offset += 4

	// Write state
	p.data[offset] = uint8(entry.State)
	offset++

	// Write created at
	binary.LittleEndian.PutUint64(p.data[offset:], uint64(entry.CreatedAt.UnixNano()))
	offset += 8

	// Write block ref count
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(entry.BlockRefs)))
	offset += 2

	// Write block refs
	for _, ref := range entry.BlockRefs {
		p.data[offset] = uint8(len(ref.ID))
		offset++
		copy(p.data[offset:], ref.ID)
		offset += uint64(len(ref.ID))
		binary.LittleEndian.PutUint32(p.data[offset:], ref.Size)
		offset += 4
	}

	// Write data
	copy(p.data[offset:], entry.Data)
	offset += uint64(len(entry.Data))

	// Update header in memory and persist to mmap region immediately
	// This ensures crash safety - even without explicit Sync(), the header
	// is always consistent with the data in the mmap file.
	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.TotalDataSize += uint64(len(entry.Data))
	p.writeHeader() // Write header to mmap region immediately for crash safety
	p.dirty = false // Header is now in sync with mmap region

	return nil
}

// AppendRemove appends a file removal entry to the WAL.
func (p *MmapPersister) AppendRemove(payloadID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	entrySize := 1 + 2 + len(payloadID)

	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	// Write entry type
	p.data[offset] = entryTypeRemove
	offset++

	// Write payload ID
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(payloadID)))
	offset += 2
	copy(p.data[offset:], []byte(payloadID))
	offset += uint64(len(payloadID))

	// Update header in memory and persist to mmap region immediately
	p.header.NextOffset = offset
	p.header.EntryCount++
	p.writeHeader() // Write header to mmap region immediately for crash safety
	p.dirty = false

	return nil
}

// Sync forces pending writes to disk.
func (p *MmapPersister) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	if !p.dirty {
		return nil
	}

	// Write header to mmap region before sync
	p.writeHeader()

	// Use MS_ASYNC for performance - data is in mmap so it's crash-safe
	if err := unix.Msync(p.data, unix.MS_ASYNC); err != nil {
		return fmt.Errorf("msync: %w", err)
	}

	p.dirty = false
	return nil
}

// Recover replays the WAL and returns all slice entries.
//
// The WAL is scanned sequentially. Remove entries mark files for exclusion,
// and slice entries for non-removed files are returned. This allows the cache
// to be reconstructed from the WAL after a crash.
func (p *MmapPersister) Recover() ([]SliceEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, ErrPersisterClosed
	}

	// First pass: collect all removed payload IDs
	removedFiles := make(map[string]bool)
	offset := uint64(mmapHeaderSize)
	endOffset := p.header.NextOffset

	for offset < endOffset {
		if offset >= p.size {
			return nil, ErrCorrupted
		}

		entryType := p.data[offset]
		offset++

		switch entryType {
		case entryTypeSlice:
			// Skip slice entry (we'll read it in second pass)
			newOffset, err := p.skipSliceEntry(offset)
			if err != nil {
				return nil, err
			}
			offset = newOffset

		case entryTypeRemove:
			payloadID, newOffset, err := p.readRemoveEntry(offset)
			if err != nil {
				return nil, err
			}
			removedFiles[payloadID] = true
			offset = newOffset

		default:
			return nil, fmt.Errorf("unknown entry type: %d", entryType)
		}
	}

	// Second pass: collect slice entries for non-removed files
	var entries []SliceEntry
	offset = uint64(mmapHeaderSize)

	for offset < endOffset {
		entryType := p.data[offset]
		offset++

		switch entryType {
		case entryTypeSlice:
			entry, newOffset, err := p.readSliceEntry(offset)
			if err != nil {
				return nil, err
			}
			if !removedFiles[entry.PayloadID] {
				entries = append(entries, *entry)
			}
			offset = newOffset

		case entryTypeRemove:
			_, newOffset, err := p.readRemoveEntry(offset)
			if err != nil {
				return nil, err
			}
			offset = newOffset
		}
	}

	return entries, nil
}

// readSliceEntry reads a slice entry from the log.
func (p *MmapPersister) readSliceEntry(offset uint64) (*SliceEntry, uint64, error) {
	entry := &SliceEntry{}

	// Read payload ID length
	if offset+2 > p.size {
		return nil, 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Read payload ID
	if offset+uint64(handleLen) > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.PayloadID = string(p.data[offset : offset+uint64(handleLen)])
	offset += uint64(handleLen)

	// Read chunk index
	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.ChunkIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read slice ID (36 bytes)
	if offset+36 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.ID = string(p.data[offset : offset+36])
	offset += 36

	// Read offset in chunk
	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.Offset = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read data length
	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.Length = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read state
	if offset+1 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.State = SliceState(p.data[offset])
	offset++

	// Read created at
	if offset+8 > p.size {
		return nil, 0, ErrCorrupted
	}
	createdAtNano := int64(binary.LittleEndian.Uint64(p.data[offset:]))
	entry.CreatedAt = time.Unix(0, createdAtNano)
	offset += 8

	// Read block ref count
	if offset+2 > p.size {
		return nil, 0, ErrCorrupted
	}
	blockRefCount := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Read block refs
	entry.BlockRefs = make([]BlockRef, blockRefCount)
	for i := range blockRefCount {
		if offset+1 > p.size {
			return nil, 0, ErrCorrupted
		}
		idLen := p.data[offset]
		offset++

		if offset+uint64(idLen) > p.size {
			return nil, 0, ErrCorrupted
		}
		entry.BlockRefs[i].ID = string(p.data[offset : offset+uint64(idLen)])
		offset += uint64(idLen)

		if offset+4 > p.size {
			return nil, 0, ErrCorrupted
		}
		entry.BlockRefs[i].Size = binary.LittleEndian.Uint32(p.data[offset:])
		offset += 4
	}

	// Read data
	if offset+uint64(entry.Length) > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.Data = make([]byte, entry.Length)
	copy(entry.Data, p.data[offset:offset+uint64(entry.Length)])
	offset += uint64(entry.Length)

	return entry, offset, nil
}

// skipSliceEntry skips a slice entry without reading its data.
// Used in the first pass of recovery to find removed files.
func (p *MmapPersister) skipSliceEntry(offset uint64) (uint64, error) {
	// Skip payload ID
	if offset+2 > p.size {
		return 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2 + uint64(handleLen)

	// Skip chunk index (4) + slice ID (36) + offset (4) + length (4) + state (1) + created at (8)
	fixedSize := uint64(4 + 36 + 4 + 4 + 1 + 8)
	if offset+fixedSize > p.size {
		return 0, ErrCorrupted
	}

	// Read length to know how much data to skip
	dataLen := binary.LittleEndian.Uint32(p.data[offset+4+36+4:])
	offset += fixedSize

	// Skip block refs
	if offset+2 > p.size {
		return 0, ErrCorrupted
	}
	blockRefCount := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	for range blockRefCount {
		if offset+1 > p.size {
			return 0, ErrCorrupted
		}
		idLen := p.data[offset]
		offset += 1 + uint64(idLen) + 4 // ID length + ID + Size
	}

	// Skip data
	offset += uint64(dataLen)

	return offset, nil
}

// readRemoveEntry reads a file removal entry from the log.
func (p *MmapPersister) readRemoveEntry(offset uint64) (string, uint64, error) {
	// Read payload ID length
	if offset+2 > p.size {
		return "", 0, ErrCorrupted
	}
	idLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Read payload ID
	if offset+uint64(idLen) > p.size {
		return "", 0, ErrCorrupted
	}
	payloadID := string(p.data[offset : offset+uint64(idLen)])
	offset += uint64(idLen)

	return payloadID, offset, nil
}

// Close releases resources held by the persister.
func (p *MmapPersister) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.closeLocked()
}

// closeLocked closes the persister (caller must hold lock).
func (p *MmapPersister) closeLocked() error {
	if p.closed {
		return nil
	}

	p.closed = true

	if p.data != nil {
		// Write header before final sync
		if p.dirty {
			p.writeHeader()
		}

		// Sync before close
		_ = unix.Msync(p.data, unix.MS_SYNC)

		if err := unix.Munmap(p.data); err != nil {
			return fmt.Errorf("munmap: %w", err)
		}
		p.data = nil
	}

	if p.file != nil {
		if err := p.file.Close(); err != nil {
			return fmt.Errorf("close file: %w", err)
		}
		p.file = nil
	}

	return nil
}

// IsEnabled returns true (mmap persistence is enabled).
func (p *MmapPersister) IsEnabled() bool {
	return true
}

// writeHeader writes the current header to the mmap file.
func (p *MmapPersister) writeHeader() {
	copy(p.data[headerOffsetMagic:], p.header.Magic[:])
	binary.LittleEndian.PutUint16(p.data[headerOffsetVersion:], p.header.Version)
	binary.LittleEndian.PutUint32(p.data[headerOffsetEntryCount:], p.header.EntryCount)
	binary.LittleEndian.PutUint64(p.data[headerOffsetNextOffset:], p.header.NextOffset)
	binary.LittleEndian.PutUint64(p.data[headerOffsetTotalDataSize:], p.header.TotalDataSize)
}

// ensureSpace ensures there's enough space in the mmap region.
func (p *MmapPersister) ensureSpace(needed uint64) error {
	if p.header.NextOffset+needed <= p.size {
		return nil
	}

	// Need to grow
	newSize := p.size * mmapGrowthFactor
	for p.header.NextOffset+needed > newSize {
		newSize *= mmapGrowthFactor
	}

	// Unmap current region
	if err := unix.Munmap(p.data); err != nil {
		return fmt.Errorf("munmap: %w", err)
	}

	// Extend file
	if err := p.file.Truncate(int64(newSize)); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	// Remap
	data, err := unix.Mmap(int(p.file.Fd()), 0, int(newSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}

	p.data = data
	p.size = newSize

	return nil
}
