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
	mmapGrowthFactor = 2                 // Double size when growing
)

// Entry types for the append-only log
const (
	entryTypeSlice  uint8 = 0
	entryTypeDelete uint8 = 1
	entryTypeRemove uint8 = 3
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
	mu      sync.Mutex
	path    string
	file    *os.File
	data    []byte // mmap'd region
	size    uint64 // current file/mmap size
	header  *mmapHeader
	dirty   bool
	closed  bool
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
	copy(header.Magic[:], data[0:4])
	header.Version = binary.LittleEndian.Uint16(data[4:6])
	header.EntryCount = binary.LittleEndian.Uint32(data[6:10])
	header.NextOffset = binary.LittleEndian.Uint64(data[10:18])
	header.TotalDataSize = binary.LittleEndian.Uint64(data[18:26])

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
		2 + len(entry.FileHandle) + // file handle
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

	// Write file handle
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(entry.FileHandle)))
	offset += 2
	copy(p.data[offset:], entry.FileHandle)
	offset += uint64(len(entry.FileHandle))

	// Write chunk index
	binary.LittleEndian.PutUint32(p.data[offset:], entry.ChunkIdx)
	offset += 4

	// Write slice ID (pad to 36 bytes)
	idBytes := []byte(entry.SliceID)
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

	// Update header
	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.TotalDataSize += uint64(len(entry.Data))
	p.writeHeader()

	p.dirty = true

	return nil
}

// AppendRemove appends a file removal entry to the WAL.
func (p *MmapPersister) AppendRemove(fileHandle []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	entrySize := 1 + 2 + len(fileHandle)

	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	// Write entry type
	p.data[offset] = entryTypeRemove
	offset++

	// Write file handle
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(fileHandle)))
	offset += 2
	copy(p.data[offset:], fileHandle)
	offset += uint64(len(fileHandle))

	// Update header
	p.header.NextOffset = offset
	p.header.EntryCount++
	p.writeHeader()

	p.dirty = true

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

	// Use MS_ASYNC for performance - data is in mmap so it's crash-safe
	if err := unix.Msync(p.data, unix.MS_ASYNC); err != nil {
		return fmt.Errorf("msync: %w", err)
	}

	p.dirty = false
	return nil
}

// Recover replays the WAL and returns all slice entries.
func (p *MmapPersister) Recover() ([]SliceEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, ErrPersisterClosed
	}

	var entries []SliceEntry
	removedFiles := make(map[string]bool)

	offset := uint64(mmapHeaderSize)
	endOffset := p.header.NextOffset

	for offset < endOffset {
		if offset+1 > p.size {
			return nil, ErrCorrupted
		}

		entryType := p.data[offset]
		offset++

		switch entryType {
		case entryTypeSlice:
			entry, newOffset, err := p.readSliceEntry(offset)
			if err != nil {
				return nil, err
			}
			// Only add if file wasn't removed later
			if !removedFiles[string(entry.FileHandle)] {
				entries = append(entries, *entry)
			}
			offset = newOffset

		case entryTypeDelete:
			newOffset, err := p.skipDeleteEntry(offset)
			if err != nil {
				return nil, err
			}
			offset = newOffset

		case entryTypeRemove:
			fileHandle, newOffset, err := p.readRemoveEntry(offset)
			if err != nil {
				return nil, err
			}
			removedFiles[string(fileHandle)] = true
			offset = newOffset

		default:
			return nil, fmt.Errorf("unknown entry type: %d", entryType)
		}
	}

	// Filter out entries for removed files
	var filteredEntries []SliceEntry
	for _, entry := range entries {
		if !removedFiles[string(entry.FileHandle)] {
			filteredEntries = append(filteredEntries, entry)
		}
	}

	return filteredEntries, nil
}

// readSliceEntry reads a slice entry from the log.
func (p *MmapPersister) readSliceEntry(offset uint64) (*SliceEntry, uint64, error) {
	entry := &SliceEntry{}

	// Read file handle length
	if offset+2 > p.size {
		return nil, 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Read file handle
	if offset+uint64(handleLen) > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.FileHandle = make([]byte, handleLen)
	copy(entry.FileHandle, p.data[offset:offset+uint64(handleLen)])
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
	entry.SliceID = string(p.data[offset : offset+36])
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
	for i := uint16(0); i < blockRefCount; i++ {
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

// skipDeleteEntry skips a delete entry in the log.
func (p *MmapPersister) skipDeleteEntry(offset uint64) (uint64, error) {
	// Read file handle length
	if offset+2 > p.size {
		return 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Skip file handle
	offset += uint64(handleLen)

	// Skip slice ID (36 bytes)
	if offset+36 > p.size {
		return 0, ErrCorrupted
	}
	offset += 36

	// Skip new state (1 byte)
	if offset+1 > p.size {
		return 0, ErrCorrupted
	}
	offset++

	return offset, nil
}

// readRemoveEntry reads a file removal entry from the log.
func (p *MmapPersister) readRemoveEntry(offset uint64) ([]byte, uint64, error) {
	// Read file handle length
	if offset+2 > p.size {
		return nil, 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Read file handle
	if offset+uint64(handleLen) > p.size {
		return nil, 0, ErrCorrupted
	}
	fileHandle := make([]byte, handleLen)
	copy(fileHandle, p.data[offset:offset+uint64(handleLen)])
	offset += uint64(handleLen)

	return fileHandle, offset, nil
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
	copy(p.data[0:4], p.header.Magic[:])
	binary.LittleEndian.PutUint16(p.data[4:6], p.header.Version)
	binary.LittleEndian.PutUint32(p.data[6:10], p.header.EntryCount)
	binary.LittleEndian.PutUint64(p.data[10:18], p.header.NextOffset)
	binary.LittleEndian.PutUint64(p.data[18:26], p.header.TotalDataSize)
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

// Ensure MmapPersister implements Persister.
var _ Persister = (*MmapPersister)(nil)
