// mmap.go provides optional memory-mapped file backing for the cache.
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
//	  - Entry type: uint8 (1 byte) - 0=slice, 1=delete, 2=truncate
//	  - File handle length: uint16 (2 bytes)
//	  - File handle: variable
//	  - Chunk index: uint32 (4 bytes)
//	  - Slice ID: 36 bytes (UUID string)
//	  - Offset in chunk: uint32 (4 bytes)
//	  - Data length: uint32 (4 bytes)
//	  - State: uint8 (1 byte)
//	  - CreatedAt: int64 (8 bytes)
//	  - BlockRef count: uint16 (2 bytes)
//	  - BlockRefs: variable (ID length + ID + offset + size per ref)
//	  - Data: variable
//
// Recovery:
// On startup, the log is replayed to reconstruct in-memory state.
// Periodic compaction rewrites the log to remove obsolete entries.
package cache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// mmap-related errors
var (
	ErrMmapNotEnabled   = errors.New("mmap backing not enabled")
	ErrMmapCorrupted    = errors.New("mmap file corrupted")
	ErrMmapVersionMismatch = errors.New("mmap file version mismatch")
)

// mmap file constants
const (
	mmapMagic        = "DTTC" // DittoFS Cache
	mmapVersion      = uint16(1)
	mmapHeaderSize   = 64
	mmapInitialSize  = 64 * 1024 * 1024  // 64MB initial file size
	mmapGrowthFactor = 2                  // Double size when growing
)

// Entry types for the append-only log
const (
	entryTypeSlice    uint8 = 0
	entryTypeDelete   uint8 = 1
	entryTypeTruncate uint8 = 2
	entryTypeRemove   uint8 = 3
)

// mmapHeader represents the header of the mmap file
type mmapHeader struct {
	Magic         [4]byte
	Version       uint16
	EntryCount    uint32
	NextOffset    uint64
	TotalDataSize uint64
	// Reserved: 38 bytes (padding to 64 bytes)
}

// mmapState holds mmap-related state
type mmapState struct {
	mu       sync.Mutex
	enabled  bool
	path     string
	file     *os.File
	data     []byte // mmap'd region
	size     uint64 // current file/mmap size
	header   *mmapHeader
	dirty    bool
}

// NewWithMmap creates a new cache with mmap-backed persistence.
//
// The cache data is stored in an append-only log file at the given path.
// On startup, existing data is recovered from the log.
//
// Parameters:
//   - path: Directory path for the cache file (cache.dat will be created)
//   - maxSize: Maximum cache size in bytes (0 = unlimited)
func NewWithMmap(path string, maxSize uint64) (*Cache, error) {
	// Ensure directory exists
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}

	c := &Cache{
		files:   make(map[string]*fileEntry),
		maxSize: maxSize,
		mmap: &mmapState{
			enabled: true,
			path:    path,
		},
	}

	// Initialize or recover mmap file
	if err := c.initMmap(); err != nil {
		return nil, fmt.Errorf("init mmap: %w", err)
	}

	return c, nil
}

// initMmap initializes the mmap file, creating it if needed or recovering existing data.
func (c *Cache) initMmap() error {
	if c.mmap == nil || !c.mmap.enabled {
		return nil
	}

	c.mmap.mu.Lock()
	defer c.mmap.mu.Unlock()

	filePath := filepath.Join(c.mmap.path, "cache.dat")

	// Check if file exists
	_, err := os.Stat(filePath)
	fileExists := err == nil

	if fileExists {
		// Open existing file and recover
		return c.recoverMmap(filePath)
	}

	// Create new file
	return c.createMmap(filePath)
}

// createMmap creates a new mmap file with initial size.
func (c *Cache) createMmap(filePath string) error {
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

	c.mmap.file = f
	c.mmap.data = data
	c.mmap.size = mmapInitialSize

	// Write header
	c.mmap.header = &mmapHeader{
		Version:       mmapVersion,
		EntryCount:    0,
		NextOffset:    mmapHeaderSize,
		TotalDataSize: 0,
	}
	copy(c.mmap.header.Magic[:], mmapMagic)

	c.writeHeader()

	return nil
}

// recoverMmap opens an existing mmap file and recovers data.
func (c *Cache) recoverMmap(filePath string) error {
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
		return ErrMmapCorrupted
	}

	// Memory map
	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return fmt.Errorf("mmap: %w", err)
	}

	c.mmap.file = f
	c.mmap.data = data
	c.mmap.size = size

	// Read and validate header
	header := &mmapHeader{}
	copy(header.Magic[:], data[0:4])
	header.Version = binary.LittleEndian.Uint16(data[4:6])
	header.EntryCount = binary.LittleEndian.Uint32(data[6:10])
	header.NextOffset = binary.LittleEndian.Uint64(data[10:18])
	header.TotalDataSize = binary.LittleEndian.Uint64(data[18:26])

	if string(header.Magic[:]) != mmapMagic {
		c.closeMmapLocked()
		return ErrMmapCorrupted
	}

	if header.Version != mmapVersion {
		c.closeMmapLocked()
		return ErrMmapVersionMismatch
	}

	c.mmap.header = header

	// Replay log entries
	if err := c.replayLog(); err != nil {
		c.closeMmapLocked()
		return fmt.Errorf("replay log: %w", err)
	}

	return nil
}

// replayLog replays all log entries to reconstruct in-memory state.
func (c *Cache) replayLog() error {
	offset := uint64(mmapHeaderSize)
	endOffset := c.mmap.header.NextOffset

	for offset < endOffset {
		if offset+1 > c.mmap.size {
			return ErrMmapCorrupted
		}

		entryType := c.mmap.data[offset]
		offset++

		switch entryType {
		case entryTypeSlice:
			newOffset, err := c.replaySliceEntry(offset)
			if err != nil {
				return err
			}
			offset = newOffset

		case entryTypeDelete:
			newOffset, err := c.replayDeleteEntry(offset)
			if err != nil {
				return err
			}
			offset = newOffset

		case entryTypeRemove:
			newOffset, err := c.replayRemoveEntry(offset)
			if err != nil {
				return err
			}
			offset = newOffset

		default:
			return fmt.Errorf("unknown entry type: %d", entryType)
		}
	}

	return nil
}

// replaySliceEntry replays a slice entry from the log.
func (c *Cache) replaySliceEntry(offset uint64) (uint64, error) {
	// Read file handle length
	if offset+2 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(c.mmap.data[offset:])
	offset += 2

	// Read file handle
	if offset+uint64(handleLen) > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	fileHandle := make([]byte, handleLen)
	copy(fileHandle, c.mmap.data[offset:offset+uint64(handleLen)])
	offset += uint64(handleLen)

	// Read chunk index
	if offset+4 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	chunkIdx := binary.LittleEndian.Uint32(c.mmap.data[offset:])
	offset += 4

	// Read slice ID (36 bytes UUID string)
	if offset+36 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	sliceID := string(c.mmap.data[offset : offset+36])
	offset += 36

	// Read offset in chunk
	if offset+4 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	sliceOffset := binary.LittleEndian.Uint32(c.mmap.data[offset:])
	offset += 4

	// Read data length
	if offset+4 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	dataLen := binary.LittleEndian.Uint32(c.mmap.data[offset:])
	offset += 4

	// Read state
	if offset+1 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	state := SliceState(c.mmap.data[offset])
	offset++

	// Read created at
	if offset+8 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	createdAtNano := int64(binary.LittleEndian.Uint64(c.mmap.data[offset:]))
	createdAt := time.Unix(0, createdAtNano)
	offset += 8

	// Read block ref count
	if offset+2 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	blockRefCount := binary.LittleEndian.Uint16(c.mmap.data[offset:])
	offset += 2

	// Read block refs
	blockRefs := make([]BlockRef, blockRefCount)
	for i := uint16(0); i < blockRefCount; i++ {
		// Read block ID length
		if offset+1 > c.mmap.size {
			return 0, ErrMmapCorrupted
		}
		idLen := c.mmap.data[offset]
		offset++

		// Read block ID
		if offset+uint64(idLen) > c.mmap.size {
			return 0, ErrMmapCorrupted
		}
		blockRefs[i].ID = string(c.mmap.data[offset : offset+uint64(idLen)])
		offset += uint64(idLen)

		// Read size
		if offset+4 > c.mmap.size {
			return 0, ErrMmapCorrupted
		}
		blockRefs[i].Size = binary.LittleEndian.Uint32(c.mmap.data[offset:])
		offset += 4
	}

	// Read data
	if offset+uint64(dataLen) > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	data := make([]byte, dataLen)
	copy(data, c.mmap.data[offset:offset+uint64(dataLen)])
	offset += uint64(dataLen)

	// Add slice to in-memory cache
	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		chunk = &chunkEntry{
			slices: make([]Slice, 0),
		}
		entry.chunks[chunkIdx] = chunk
	}

	slice := Slice{
		ID:        sliceID,
		Offset:    sliceOffset,
		Length:    dataLen,
		Data:      data,
		State:     state,
		CreatedAt: createdAt,
		BlockRefs: blockRefs,
	}

	// Prepend to slices (newest first)
	chunk.slices = append([]Slice{slice}, chunk.slices...)
	c.totalSize.Add(uint64(dataLen))

	entry.mu.Unlock()

	return offset, nil
}

// replayDeleteEntry replays a delete entry (mark slice as flushed).
func (c *Cache) replayDeleteEntry(offset uint64) (uint64, error) {
	// For now, delete entries just mark state change
	// Read file handle length
	if offset+2 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(c.mmap.data[offset:])
	offset += 2

	// Skip file handle
	offset += uint64(handleLen)

	// Read slice ID
	if offset+36 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	offset += 36

	// Read new state
	if offset+1 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	offset++

	return offset, nil
}

// replayRemoveEntry replays a file removal entry.
func (c *Cache) replayRemoveEntry(offset uint64) (uint64, error) {
	// Read file handle length
	if offset+2 > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(c.mmap.data[offset:])
	offset += 2

	// Read file handle
	if offset+uint64(handleLen) > c.mmap.size {
		return 0, ErrMmapCorrupted
	}
	fileHandle := make([]byte, handleLen)
	copy(fileHandle, c.mmap.data[offset:offset+uint64(handleLen)])
	offset += uint64(handleLen)

	// Remove from in-memory cache
	key := string(fileHandle)
	c.globalMu.Lock()
	delete(c.files, key)
	c.globalMu.Unlock()

	return offset, nil
}

// writeHeader writes the current header to the mmap file.
func (c *Cache) writeHeader() {
	copy(c.mmap.data[0:4], c.mmap.header.Magic[:])
	binary.LittleEndian.PutUint16(c.mmap.data[4:6], c.mmap.header.Version)
	binary.LittleEndian.PutUint32(c.mmap.data[6:10], c.mmap.header.EntryCount)
	binary.LittleEndian.PutUint64(c.mmap.data[10:18], c.mmap.header.NextOffset)
	binary.LittleEndian.PutUint64(c.mmap.data[18:26], c.mmap.header.TotalDataSize)
}

// appendSliceEntry appends a slice entry to the log.
func (c *Cache) appendSliceEntry(fileHandle []byte, chunkIdx uint32, slice *Slice) error {
	if c.mmap == nil || !c.mmap.enabled {
		return nil
	}

	c.mmap.mu.Lock()
	defer c.mmap.mu.Unlock()

	// Calculate entry size
	entrySize := 1 + // entry type
		2 + len(fileHandle) + // file handle
		4 + // chunk index
		36 + // slice ID
		4 + // offset
		4 + // length
		1 + // state
		8 + // created at
		2 // block ref count

	for _, ref := range slice.BlockRefs {
		entrySize += 1 + len(ref.ID) + 4 // ID length + ID + Size (uint32)
	}
	entrySize += len(slice.Data)

	// Ensure space
	if err := c.ensureMmapSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := c.mmap.header.NextOffset

	// Write entry type
	c.mmap.data[offset] = entryTypeSlice
	offset++

	// Write file handle
	binary.LittleEndian.PutUint16(c.mmap.data[offset:], uint16(len(fileHandle)))
	offset += 2
	copy(c.mmap.data[offset:], fileHandle)
	offset += uint64(len(fileHandle))

	// Write chunk index
	binary.LittleEndian.PutUint32(c.mmap.data[offset:], chunkIdx)
	offset += 4

	// Write slice ID (pad to 36 bytes)
	idBytes := []byte(slice.ID)
	if len(idBytes) > 36 {
		idBytes = idBytes[:36]
	}
	copy(c.mmap.data[offset:offset+36], idBytes)
	offset += 36

	// Write offset
	binary.LittleEndian.PutUint32(c.mmap.data[offset:], slice.Offset)
	offset += 4

	// Write length
	binary.LittleEndian.PutUint32(c.mmap.data[offset:], slice.Length)
	offset += 4

	// Write state
	c.mmap.data[offset] = uint8(slice.State)
	offset++

	// Write created at
	binary.LittleEndian.PutUint64(c.mmap.data[offset:], uint64(slice.CreatedAt.UnixNano()))
	offset += 8

	// Write block ref count
	binary.LittleEndian.PutUint16(c.mmap.data[offset:], uint16(len(slice.BlockRefs)))
	offset += 2

	// Write block refs
	for _, ref := range slice.BlockRefs {
		c.mmap.data[offset] = uint8(len(ref.ID))
		offset++
		copy(c.mmap.data[offset:], ref.ID)
		offset += uint64(len(ref.ID))
		binary.LittleEndian.PutUint32(c.mmap.data[offset:], ref.Size)
		offset += 4
	}

	// Write data
	copy(c.mmap.data[offset:], slice.Data)
	offset += uint64(len(slice.Data))

	// Update header
	c.mmap.header.NextOffset = offset
	c.mmap.header.EntryCount++
	c.mmap.header.TotalDataSize += uint64(len(slice.Data))
	c.writeHeader()

	c.mmap.dirty = true

	return nil
}

// appendRemoveEntry appends a file removal entry to the log.
func (c *Cache) appendRemoveEntry(fileHandle []byte) error {
	if c.mmap == nil || !c.mmap.enabled {
		return nil
	}

	c.mmap.mu.Lock()
	defer c.mmap.mu.Unlock()

	entrySize := 1 + 2 + len(fileHandle)

	if err := c.ensureMmapSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := c.mmap.header.NextOffset

	// Write entry type
	c.mmap.data[offset] = entryTypeRemove
	offset++

	// Write file handle
	binary.LittleEndian.PutUint16(c.mmap.data[offset:], uint16(len(fileHandle)))
	offset += 2
	copy(c.mmap.data[offset:], fileHandle)
	offset += uint64(len(fileHandle))

	// Update header
	c.mmap.header.NextOffset = offset
	c.mmap.header.EntryCount++
	c.writeHeader()

	c.mmap.dirty = true

	return nil
}

// ensureMmapSpace ensures there's enough space in the mmap region.
func (c *Cache) ensureMmapSpace(needed uint64) error {
	if c.mmap.header.NextOffset+needed <= c.mmap.size {
		return nil
	}

	// Need to grow
	newSize := c.mmap.size * mmapGrowthFactor
	for c.mmap.header.NextOffset+needed > newSize {
		newSize *= mmapGrowthFactor
	}

	// Unmap current region
	if err := unix.Munmap(c.mmap.data); err != nil {
		return fmt.Errorf("munmap: %w", err)
	}

	// Extend file
	if err := c.mmap.file.Truncate(int64(newSize)); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	// Remap
	data, err := unix.Mmap(int(c.mmap.file.Fd()), 0, int(newSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}

	c.mmap.data = data
	c.mmap.size = newSize

	return nil
}

// Sync forces all pending writes to disk.
func (c *Cache) Sync() error {
	if c.mmap == nil || !c.mmap.enabled {
		return nil
	}

	c.mmap.mu.Lock()
	defer c.mmap.mu.Unlock()

	if !c.mmap.dirty {
		return nil
	}

	if err := unix.Msync(c.mmap.data, unix.MS_SYNC); err != nil {
		return fmt.Errorf("msync: %w", err)
	}

	c.mmap.dirty = false
	return nil
}

// closeMmap closes the mmap file.
func (c *Cache) closeMmap() error {
	if c.mmap == nil || !c.mmap.enabled {
		return nil
	}

	c.mmap.mu.Lock()
	defer c.mmap.mu.Unlock()

	return c.closeMmapLocked()
}

// closeMmapLocked closes the mmap file (caller must hold lock).
func (c *Cache) closeMmapLocked() error {
	if c.mmap.data != nil {
		// Sync before close
		_ = unix.Msync(c.mmap.data, unix.MS_SYNC)

		if err := unix.Munmap(c.mmap.data); err != nil {
			return fmt.Errorf("munmap: %w", err)
		}
		c.mmap.data = nil
	}

	if c.mmap.file != nil {
		if err := c.mmap.file.Close(); err != nil {
			return fmt.Errorf("close file: %w", err)
		}
		c.mmap.file = nil
	}

	return nil
}
