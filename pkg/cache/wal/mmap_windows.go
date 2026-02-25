//go:build windows

// mmap_windows.go provides memory-mapped file backing for WAL persistence on Windows.
// Uses CreateFileMapping + MapViewOfFile for cross-platform mmap support.

package wal

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MmapPersister implements WAL persistence using memory-mapped files on Windows.
type MmapPersister struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	data    []byte // mmap'd region
	size    uint64 // current file/mmap size
	header  *mmapHeader
	dirty   bool
	closed  bool
	mapping windows.Handle // file mapping handle
}

// NewMmapPersister creates a new mmap-backed persister on Windows.
func NewMmapPersister(path string) (*MmapPersister, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	p := &MmapPersister{
		path: path,
	}

	if err := p.init(); err != nil {
		return nil, fmt.Errorf("init mmap: %w", err)
	}

	return p, nil
}

func (p *MmapPersister) init() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	filePath := filepath.Join(p.path, "cache.dat")

	_, err := os.Stat(filePath)
	fileExists := err == nil

	if fileExists {
		return p.openExisting(filePath)
	}

	return p.createNew(filePath)
}

func (p *MmapPersister) createNew(filePath string) error {
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	if err := f.Truncate(int64(mmapInitialSize)); err != nil {
		_ = f.Close()
		return fmt.Errorf("truncate file: %w", err)
	}

	p.file = f
	p.size = mmapInitialSize

	if err := p.mapFile(); err != nil {
		_ = f.Close()
		return err
	}

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

func (p *MmapPersister) openExisting(filePath string) error {
	f, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

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

	p.file = f
	p.size = size

	if err := p.mapFile(); err != nil {
		_ = f.Close()
		return err
	}

	header := &mmapHeader{}
	copy(header.Magic[:], p.data[headerOffsetMagic:headerOffsetVersion])
	header.Version = binary.LittleEndian.Uint16(p.data[headerOffsetVersion:headerOffsetEntryCount])
	header.EntryCount = binary.LittleEndian.Uint32(p.data[headerOffsetEntryCount:headerOffsetNextOffset])
	header.NextOffset = binary.LittleEndian.Uint64(p.data[headerOffsetNextOffset:headerOffsetTotalDataSize])
	header.TotalDataSize = binary.LittleEndian.Uint64(p.data[headerOffsetTotalDataSize:])

	if string(header.Magic[:]) != mmapMagic {
		_ = p.closeLocked()
		return ErrCorrupted
	}

	if header.Version != mmapVersion {
		_ = p.closeLocked()
		return ErrVersionMismatch
	}

	p.header = header

	return nil
}

// mapFile creates a file mapping and maps a view of the file into memory.
func (p *MmapPersister) mapFile() error {
	handle := windows.Handle(p.file.Fd())

	// CreateFileMapping with PAGE_READWRITE
	mapping, err := windows.CreateFileMapping(
		handle,
		nil,
		windows.PAGE_READWRITE,
		uint32(p.size>>32),
		uint32(p.size),
		nil,
	)
	if err != nil {
		return fmt.Errorf("CreateFileMapping: %w", err)
	}

	// MapViewOfFile with FILE_MAP_WRITE
	addr, err := windows.MapViewOfFile(
		mapping,
		windows.FILE_MAP_WRITE,
		0,
		0,
		uintptr(p.size),
	)
	if err != nil {
		_ = windows.CloseHandle(mapping)
		return fmt.Errorf("MapViewOfFile: %w", err)
	}

	if p.size > uint64(math.MaxInt) {
		_ = windows.CloseHandle(mapping)
		return fmt.Errorf("mmap size %d exceeds maximum addressable size", p.size)
	}

	p.mapping = mapping
	p.data = unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(p.size))

	return nil
}

// unmapFile unmaps the view and closes the file mapping handle.
func (p *MmapPersister) unmapFile() error {
	if p.data != nil {
		addr := uintptr(unsafe.Pointer(&p.data[0]))
		if err := windows.UnmapViewOfFile(addr); err != nil {
			return fmt.Errorf("UnmapViewOfFile: %w", err)
		}
		p.data = nil
	}

	if p.mapping != 0 {
		if err := windows.CloseHandle(p.mapping); err != nil {
			return fmt.Errorf("CloseHandle mapping: %w", err)
		}
		p.mapping = 0
	}

	return nil
}

// AppendBlockWrite appends a block write entry to the WAL.
func (p *MmapPersister) AppendBlockWrite(entry *BlockWriteEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	entrySize := 1 + 2 + len(entry.PayloadID) + 4 + 4 + 4 + 4 + len(entry.Data)

	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	p.data[offset] = entryTypeBlockWrite
	offset++

	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(entry.PayloadID)))
	offset += 2
	copy(p.data[offset:], []byte(entry.PayloadID))
	offset += uint64(len(entry.PayloadID))

	binary.LittleEndian.PutUint32(p.data[offset:], entry.ChunkIdx)
	offset += 4

	binary.LittleEndian.PutUint32(p.data[offset:], entry.BlockIdx)
	offset += 4

	binary.LittleEndian.PutUint32(p.data[offset:], entry.OffsetInBlock)
	offset += 4

	binary.LittleEndian.PutUint32(p.data[offset:], uint32(len(entry.Data)))
	offset += 4

	copy(p.data[offset:], entry.Data)
	offset += uint64(len(entry.Data))

	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.TotalDataSize += uint64(len(entry.Data))
	p.header.Version = mmapVersion
	p.writeHeader()
	p.dirty = true // Data written to mmap region but not yet flushed to disk

	return nil
}

// AppendBlockUploaded appends a "block uploaded" marker to the WAL.
func (p *MmapPersister) AppendBlockUploaded(payloadID string, chunkIdx, blockIdx uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	entrySize := 1 + 2 + len(payloadID) + 4 + 4

	if err := p.ensureSpace(uint64(entrySize)); err != nil {
		return err
	}

	offset := p.header.NextOffset

	p.data[offset] = entryTypeBlockUploaded
	offset++

	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(payloadID)))
	offset += 2
	copy(p.data[offset:], []byte(payloadID))
	offset += uint64(len(payloadID))

	binary.LittleEndian.PutUint32(p.data[offset:], chunkIdx)
	offset += 4

	binary.LittleEndian.PutUint32(p.data[offset:], blockIdx)
	offset += 4

	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.Version = mmapVersion
	p.writeHeader()
	p.dirty = true // Data written to mmap region but not yet flushed to disk

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

	p.data[offset] = entryTypeRemove
	offset++

	binary.LittleEndian.PutUint16(p.data[offset:], uint16(len(payloadID)))
	offset += 2
	copy(p.data[offset:], []byte(payloadID))
	offset += uint64(len(payloadID))

	p.header.NextOffset = offset
	p.header.EntryCount++
	p.header.Version = mmapVersion
	p.writeHeader()
	p.dirty = true // Data written to mmap region but not yet flushed to disk

	return nil
}

// Sync forces pending writes to disk using FlushViewOfFile.
func (p *MmapPersister) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrPersisterClosed
	}

	if !p.dirty {
		return nil
	}

	p.writeHeader()

	addr := uintptr(unsafe.Pointer(&p.data[0]))
	if err := windows.FlushViewOfFile(addr, uintptr(p.size)); err != nil {
		return fmt.Errorf("FlushViewOfFile: %w", err)
	}

	p.dirty = false
	return nil
}

// Recover replays the WAL and returns all block write entries.
func (p *MmapPersister) Recover() (*RecoveryResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, ErrPersisterClosed
	}

	removedFiles := make(map[string]bool)
	uploadedBlocks := make(map[BlockKey]bool)
	offset := uint64(mmapHeaderSize)
	endOffset := p.header.NextOffset

	// First pass: collect removed files and uploaded blocks
	for offset < endOffset {
		if offset >= p.size {
			return nil, ErrCorrupted
		}

		entryType := p.data[offset]
		offset++

		switch entryType {
		case entryTypeBlockWrite:
			newOffset, err := p.skipBlockWriteEntry(offset)
			if err != nil {
				return nil, err
			}
			offset = newOffset

		case entryTypeBlockUploaded:
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

// Close releases resources held by the persister.
func (p *MmapPersister) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.closeLocked()
}

func (p *MmapPersister) closeLocked() error {
	if p.closed {
		return nil
	}

	p.closed = true

	if p.data != nil {
		if p.dirty {
			p.writeHeader()
		}

		// Flush to disk before closing
		addr := uintptr(unsafe.Pointer(&p.data[0]))
		_ = windows.FlushViewOfFile(addr, uintptr(p.size))
	}

	if err := p.unmapFile(); err != nil {
		return err
	}

	if p.file != nil {
		if err := p.file.Close(); err != nil {
			return fmt.Errorf("close file: %w", err)
		}
		p.file = nil
	}

	return nil
}

// IsEnabled returns true (mmap persistence is enabled on Windows).
func (p *MmapPersister) IsEnabled() bool {
	return true
}

func (p *MmapPersister) writeHeader() {
	copy(p.data[headerOffsetMagic:], p.header.Magic[:])
	binary.LittleEndian.PutUint16(p.data[headerOffsetVersion:], p.header.Version)
	binary.LittleEndian.PutUint32(p.data[headerOffsetEntryCount:], p.header.EntryCount)
	binary.LittleEndian.PutUint64(p.data[headerOffsetNextOffset:], p.header.NextOffset)
	binary.LittleEndian.PutUint64(p.data[headerOffsetTotalDataSize:], p.header.TotalDataSize)
}

func (p *MmapPersister) ensureSpace(needed uint64) error {
	if p.header.NextOffset+needed <= p.size {
		return nil
	}

	newSize := p.size * mmapGrowthFactor
	for p.header.NextOffset+needed > newSize {
		newSize *= mmapGrowthFactor
	}

	// Unmap current region
	if err := p.unmapFile(); err != nil {
		return err
	}

	// Extend file
	if err := p.file.Truncate(int64(newSize)); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	p.size = newSize

	// Remap
	return p.mapFile()
}

// readBlockWriteEntry reads a block write entry from the log.
func (p *MmapPersister) readBlockWriteEntry(offset uint64) (*BlockWriteEntry, uint64, error) {
	entry := &BlockWriteEntry{}

	if offset+2 > p.size {
		return nil, 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	if offset+uint64(handleLen) > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.PayloadID = string(p.data[offset : offset+uint64(handleLen)])
	offset += uint64(handleLen)

	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.ChunkIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.BlockIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.OffsetInBlock = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	if offset+4 > p.size {
		return nil, 0, ErrCorrupted
	}
	dataLen := binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	if offset+uint64(dataLen) > p.size {
		return nil, 0, ErrCorrupted
	}
	entry.Data = make([]byte, dataLen)
	copy(entry.Data, p.data[offset:offset+uint64(dataLen)])
	offset += uint64(dataLen)

	return entry, offset, nil
}

func (p *MmapPersister) skipBlockWriteEntry(offset uint64) (uint64, error) {
	if offset+2 > p.size {
		return 0, ErrCorrupted
	}
	handleLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2 + uint64(handleLen)

	fixedSize := uint64(16)
	if offset+fixedSize > p.size {
		return 0, ErrCorrupted
	}

	dataLen := binary.LittleEndian.Uint32(p.data[offset+12:])
	offset += fixedSize
	offset += uint64(dataLen)

	return offset, nil
}

func (p *MmapPersister) readRemoveEntry(offset uint64) (string, uint64, error) {
	if offset+2 > p.size {
		return "", 0, ErrCorrupted
	}
	idLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	if offset+uint64(idLen) > p.size {
		return "", 0, ErrCorrupted
	}
	payloadID := string(p.data[offset : offset+uint64(idLen)])
	offset += uint64(idLen)

	return payloadID, offset, nil
}

func (p *MmapPersister) readBlockUploadedEntry(offset uint64) (BlockKey, uint64, error) {
	var key BlockKey

	if offset+2 > p.size {
		return key, 0, ErrCorrupted
	}
	idLen := binary.LittleEndian.Uint16(p.data[offset:])
	offset += 2

	if offset+uint64(idLen) > p.size {
		return key, 0, ErrCorrupted
	}
	key.PayloadID = string(p.data[offset : offset+uint64(idLen)])
	offset += uint64(idLen)

	if offset+4 > p.size {
		return key, 0, ErrCorrupted
	}
	key.ChunkIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	if offset+4 > p.size {
		return key, 0, ErrCorrupted
	}
	key.BlockIdx = binary.LittleEndian.Uint32(p.data[offset:])
	offset += 4

	return key, offset, nil
}
