//go:build !windows

// mmap.go provides memory-mapped file backing for WAL persistence.
//
// When mmap backing is enabled, cache data is persisted to disk and can
// survive process restarts. The OS handles flushing dirty pages asynchronously,
// so write performance remains similar to pure in-memory operation.
//
// File Format (Block-Level WAL):
// The mmap file uses an append-only log format for crash safety:
//
//	Header (64 bytes):
//	  - Magic: "DTTC" (4 bytes)
//	  - Version: uint16 (2 bytes) - version 2 for block-level format
//	  - Entry count: uint32 (4 bytes)
//	  - Next write offset: uint64 (8 bytes)
//	  - Total data size: uint64 (8 bytes)
//	  - Reserved: 38 bytes
//
//	Block Write Entry (variable):
//	  - Entry type: uint8 (1 byte) - 0=blockWrite, 3=remove
//	  - Payload ID length: uint16 (2 bytes)
//	  - Payload ID: variable
//	  - Chunk index: uint32 (4 bytes)
//	  - Block index: uint32 (4 bytes)
//	  - Offset in block: uint32 (4 bytes)
//	  - Data length: uint32 (4 bytes)
//	  - Data: variable
//
// Recovery:
// On startup, the log is replayed to return all block write entries.

package wal

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// MmapPersister implements WAL persistence using memory-mapped files.
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
		_ = f.Close()
		return fmt.Errorf("truncate file: %w", err)
	}

	// Memory map
	data, err := unix.Mmap(int(f.Fd()), 0, mmapInitialSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
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
		_ = f.Close()
		return fmt.Errorf("stat file: %w", err)
	}

	size := uint64(info.Size())
	if size < mmapHeaderSize {
		_ = f.Close()
		return ErrCorrupted
	}

	// Memory map
	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
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
		_ = p.closeLocked()
		return ErrCorrupted
	}

	// Only the current version is supported for recovery.
	// Legacy version 1 WAL files cannot be recovered - they must be
	// discarded (the data should already be in S3 from previous runs).
	if header.Version != mmapVersion {
		_ = p.closeLocked()
		return ErrVersionMismatch
	}

	p.header = header

	return nil
}

// AppendBlockWrite appends a block write entry to the WAL.
func (p *MmapPersister) AppendBlockWrite(entry *BlockWriteEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	// Calculate entry size
	entrySize := 1 + // entry type
		2 + len(entry.PayloadID) + // payload ID
		4 + // chunk index
		4 + // block index
		4 + // offset in block
		4 + // data length
		len(entry.Data) // data

	// Ensure space
	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	// Write entry type
	p.data[offset] = entryTypeBlockWrite
	offset++

	// Write payload ID
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(entry.PayloadID)))
	offset += 2
	copy(p.data[offset:], []byte(entry.PayloadID))
	offset += uint64(len(entry.PayloadID))

	// Write chunk index
	binary.LittleEndian.PutUint32(p.data[offset:], entry.ChunkIdx)
	offset += 4

	// Write block index
	binary.LittleEndian.PutUint32(p.data[offset:], entry.BlockIdx)
	offset += 4

	// Write offset in block
	binary.LittleEndian.PutUint32(p.data[offset:], entry.OffsetInBlock)
	offset += 4

	// Write data length
	binary.LittleEndian.PutUint32(p.data[offset:], uint32(len(entry.Data)))
	offset += 4

	// Write data
	copy(p.data[offset:], entry.Data)
	offset += uint64(len(entry.Data))

	// Update header in memory and persist to mmap region immediately
	// This ensures crash safety - even without explicit Sync(), the header
	// is always consistent with the data in the mmap file.
	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.TotalDataSize += uint64(len(entry.Data))
	p.header.Version = mmapVersion // Upgrade version on write
	p.writeHeader()                // Write header to mmap region immediately for crash safety
	p.dirty = false                // Header is now in sync with mmap region

	return nil
}

// AppendBlockUploaded appends a "block uploaded" marker to the WAL.
// This indicates that the block has been successfully uploaded to S3.
// On recovery, blocks with this marker will be marked as Uploaded (evictable)
// instead of Pending, preventing unnecessary re-uploads.
//
// Entry format:
//   - Entry type: 1 byte (entryTypeBlockUploaded)
//   - Payload ID length: 2 bytes
//   - Payload ID: variable
//   - Chunk index: 4 bytes
//   - Block index: 4 bytes
func (p *MmapPersister) AppendBlockUploaded(payloadID string, chunkIdx, blockIdx uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	// Calculate entry size: type(1) + payloadIDLen(2) + payloadID + chunkIdx(4) + blockIdx(4)
	entrySize := 1 + 2 + len(payloadID) + 4 + 4

	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	// Write entry type
	p.data[offset] = entryTypeBlockUploaded
	offset++

	// Write payload ID
	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(payloadID)))
	offset += 2
	copy(p.data[offset:], []byte(payloadID))
	offset += uint64(len(payloadID))

	// Write chunk index
	binary.LittleEndian.PutUint32(p.data[offset:], chunkIdx)
	offset += 4

	// Write block index
	binary.LittleEndian.PutUint32(p.data[offset:], blockIdx)
	offset += 4

	// Update header
	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.Version = mmapVersion
	p.writeHeader()
	p.dirty = false

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
	p.header.Version = mmapVersion // Upgrade version on write
	p.writeHeader()                // Write header to mmap region immediately for crash safety
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

// Recover replays the WAL and returns all block write entries along with
// information about which blocks were already uploaded to S3.
//
// The WAL is scanned sequentially. Remove entries mark files for exclusion,
// block write entries for non-removed files are collected, and block uploaded
// entries track which blocks are safe in S3.
//
// Returns RecoveryResult containing:
//   - Entries: Block write entries to replay into cache
//   - UploadedBlocks: Map of blocks that were uploaded to S3 (should be marked
//     as Uploaded, not Pending, to avoid re-upload)
func (p *MmapPersister) Recover() (*RecoveryResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, ErrPersisterClosed
	}

	// First pass: collect all removed payload IDs and uploaded blocks
	removedFiles := make(map[string]bool)
	uploadedBlocks := make(map[BlockKey]bool)
	offset := uint64(mmapHeaderSize)
	endOffset := p.header.NextOffset

	for offset < endOffset {
		if offset >= p.size {
			return nil, ErrCorrupted
		}

		entryType := p.data[offset]
		offset++

		switch entryType {
		case entryTypeBlockWrite:
			// Skip block write entry (we'll read it in second pass)
			newOffset, err := p.skipBlockWriteEntry(offset)
			if err != nil {
				return nil, err
			}
			offset = newOffset

		case entryTypeBlockUploaded:
			// Track uploaded block
			key, newOffset, err := p.readBlockUploadedEntry(offset)
			if err != nil {
				return nil, err
			}
			uploadedBlocks[key] = true
			offset = newOffset

		case entryTypeRemove:
			payloadID, newOffset, err := p.readRemoveEntry(offset)
			if err != nil {
				return nil, err
			}
			removedFiles[payloadID] = true
			// Also remove any uploaded blocks for this file
			for key := range uploadedBlocks {
				if key.PayloadID == payloadID {
					delete(uploadedBlocks, key)
				}
			}
			offset = newOffset

		default:
			return nil, fmt.Errorf("unknown entry type: %d", entryType)
		}
	}

	// Second pass: collect block write entries for non-removed files
	var entries []BlockWriteEntry
	offset = uint64(mmapHeaderSize)

	for offset < endOffset {
		entryType := p.data[offset]
		offset++

		switch entryType {
		case entryTypeBlockWrite:
			entry, newOffset, err := p.readBlockWriteEntry(offset)
			if err != nil {
				return nil, err
			}
			if !removedFiles[entry.PayloadID] {
				entries = append(entries, *entry)
			}
			offset = newOffset

		case entryTypeBlockUploaded:
			// Skip - already processed in first pass
			_, newOffset, err := p.readBlockUploadedEntry(offset)
			if err != nil {
				return nil, err
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

	return &RecoveryResult{
		Entries:        entries,
		UploadedBlocks: uploadedBlocks,
	}, nil
}

// readBlockWriteEntry reads a block write entry from the log.
func (p *MmapPersister) readBlockWriteEntry(offset uint64) (*BlockWriteEntry, uint64, error) {
	entry := &BlockWriteEntry{}

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

	// Read block index
	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.BlockIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read offset in block
	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.OffsetInBlock = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read data length
	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	dataLen := binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read data
	if offset+uint64(dataLen) > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.Data = make([]byte, dataLen)
	copy(entry.Data, p.data[offset:offset+uint64(dataLen)])
	offset += uint64(dataLen)

	return entry, offset, nil
}

// skipBlockWriteEntry skips a block write entry without reading its data.
// Used in the first pass of recovery to find removed files.
func (p *MmapPersister) skipBlockWriteEntry(offset uint64) (uint64, error) {
	// Skip payload ID
	if offset+2 > p.size {
		return 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2 + uint64(handleLen)

	// Skip chunk index (4) + block index (4) + offset in block (4) + data length (4)
	fixedSize := uint64(16)
	if offset+fixedSize > p.size {
		return 0, ErrCorrupted
	}

	// Read data length to know how much to skip
	dataLen := binary.LittleEndian.Uint32(p.data[offset+12:])
	offset += fixedSize

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

// readBlockUploadedEntry reads a block uploaded marker from the log.
// Returns the BlockKey identifying the uploaded block.
func (p *MmapPersister) readBlockUploadedEntry(offset uint64) (BlockKey, uint64, error) {
	var key BlockKey

	// Read payload ID length
	if offset+2 > p.size {
		return key, 0, ErrCorrupted
	}
	idLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	// Read payload ID
	if offset+uint64(idLen) > p.size {
		return key, 0, ErrCorrupted
	}
	key.PayloadID = string(p.data[offset : offset+uint64(idLen)])
	offset += uint64(idLen)

	// Read chunk index
	if offset+4 > p.size {
		return key, 0, ErrCorrupted
	}
	key.ChunkIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	// Read block index
	if offset+4 > p.size {
		return key, 0, ErrCorrupted
	}
	key.BlockIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	return key, offset, nil
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
