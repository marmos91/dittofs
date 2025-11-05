package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// SetAttrRequest represents a SETATTR request
type SetAttrRequest struct {
	Handle  []byte
	NewAttr SetAttrs
	Guard   TimeGuard
}

// SetAttrs contains the attributes to set
type SetAttrs struct {
	SetMode  bool
	Mode     uint32
	SetUID   bool
	UID      uint32
	SetGID   bool
	GID      uint32
	SetSize  bool
	Size     uint64
	SetAtime bool
	Atime    TimeVal
	SetMtime bool
	Mtime    TimeVal
}

// TimeGuard is used for conditional updates
type TimeGuard struct {
	Check bool
	Time  TimeVal
}

// SetAttrResponse represents a SETATTR response
type SetAttrResponse struct {
	Status     uint32
	AttrBefore *WccAttr  // Pre-op attributes (optional)
	AttrAfter  *FileAttr // Post-op attributes (optional)
}

// SetAttr sets the attributes for a file system object.
// RFC 1813 Section 3.3.2
func (h *DefaultNFSHandler) SetAttr(repository metadata.Repository, req *SetAttrRequest) (*SetAttrResponse, error) {
	logger.Debug("SETATTR for handle: %x", req.Handle)

	// Get current file attributes
	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Warn("File not found: %v", err)
		return &SetAttrResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Store pre-op attributes for WCC
	wccAttr := &WccAttr{
		Size: attr.Size,
		Mtime: TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}

	// Check guard if present
	if req.Guard.Check {
		currentCtime := TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		}
		if currentCtime.Seconds != req.Guard.Time.Seconds ||
			currentCtime.Nseconds != req.Guard.Time.Nseconds {
			logger.Debug("Guard check failed")
			return &SetAttrResponse{
				Status:     NFS3ErrNotSync,
				AttrBefore: wccAttr,
			}, nil
		}
	}

	// Apply new attributes
	modified := false

	if req.NewAttr.SetMode {
		attr.Mode = req.NewAttr.Mode
		modified = true
		logger.Debug("Setting mode to %o", req.NewAttr.Mode)
	}

	if req.NewAttr.SetUID {
		attr.UID = req.NewAttr.UID
		modified = true
		logger.Debug("Setting UID to %d", req.NewAttr.UID)
	}

	if req.NewAttr.SetGID {
		attr.GID = req.NewAttr.GID
		modified = true
		logger.Debug("Setting GID to %d", req.NewAttr.GID)
	}

	if req.NewAttr.SetSize {
		// Note: Changing size would require content repository integration
		// For now, we'll just update the metadata
		attr.Size = req.NewAttr.Size
		modified = true
		logger.Debug("Setting size to %d", req.NewAttr.Size)
	}

	if req.NewAttr.SetAtime {
		attr.Atime = time.Unix(int64(req.NewAttr.Atime.Seconds), int64(req.NewAttr.Atime.Nseconds))
		modified = true
		logger.Debug("Setting atime")
	}

	if req.NewAttr.SetMtime {
		attr.Mtime = time.Unix(int64(req.NewAttr.Mtime.Seconds), int64(req.NewAttr.Mtime.Nseconds))
		modified = true
		logger.Debug("Setting mtime")
	}

	// Update ctime if anything was modified
	if modified {
		attr.Ctime = time.Now()
	}

	// Save updated attributes
	if err := repository.UpdateFile(metadata.FileHandle(req.Handle), attr); err != nil {
		logger.Error("Failed to update file: %v", err)
		return &SetAttrResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	logger.Info("SETATTR successful")

	return &SetAttrResponse{
		Status:     NFS3OK,
		AttrBefore: wccAttr,
		AttrAfter:  nfsAttr,
	}, nil
}

func DecodeSetAttrRequest(data []byte) (*SetAttrRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short")
	}

	reader := bytes.NewReader(data)

	// Read handle length
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Read handle
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	// Skip padding
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		reader.ReadByte()
	}

	// Read new attributes
	newAttr := SetAttrs{}

	// Mode
	var setMode uint32
	if err := binary.Read(reader, binary.BigEndian, &setMode); err != nil {
		return nil, fmt.Errorf("read set_mode: %w", err)
	}
	newAttr.SetMode = (setMode == 1)
	if newAttr.SetMode {
		if err := binary.Read(reader, binary.BigEndian, &newAttr.Mode); err != nil {
			return nil, fmt.Errorf("read mode: %w", err)
		}
	}

	// UID
	var setUID uint32
	if err := binary.Read(reader, binary.BigEndian, &setUID); err != nil {
		return nil, fmt.Errorf("read set_uid: %w", err)
	}
	newAttr.SetUID = (setUID == 1)
	if newAttr.SetUID {
		if err := binary.Read(reader, binary.BigEndian, &newAttr.UID); err != nil {
			return nil, fmt.Errorf("read uid: %w", err)
		}
	}

	// GID
	var setGID uint32
	if err := binary.Read(reader, binary.BigEndian, &setGID); err != nil {
		return nil, fmt.Errorf("read set_gid: %w", err)
	}
	newAttr.SetGID = (setGID == 1)
	if newAttr.SetGID {
		if err := binary.Read(reader, binary.BigEndian, &newAttr.GID); err != nil {
			return nil, fmt.Errorf("read gid: %w", err)
		}
	}

	// Size
	var setSize uint32
	if err := binary.Read(reader, binary.BigEndian, &setSize); err != nil {
		return nil, fmt.Errorf("read set_size: %w", err)
	}
	newAttr.SetSize = (setSize == 1)
	if newAttr.SetSize {
		if err := binary.Read(reader, binary.BigEndian, &newAttr.Size); err != nil {
			return nil, fmt.Errorf("read size: %w", err)
		}
	}

	// Atime
	var setAtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setAtime); err != nil {
		return nil, fmt.Errorf("read set_atime: %w", err)
	}
	if setAtime == 1 {
		newAttr.SetAtime = true
		if err := binary.Read(reader, binary.BigEndian, &newAttr.Atime.Seconds); err != nil {
			return nil, fmt.Errorf("read atime seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &newAttr.Atime.Nseconds); err != nil {
			return nil, fmt.Errorf("read atime nseconds: %w", err)
		}
	} else if setAtime == 2 {
		// SET_TO_SERVER_TIME
		newAttr.SetAtime = true
		now := time.Now()
		newAttr.Atime = TimeVal{
			Seconds:  uint32(now.Unix()),
			Nseconds: uint32(now.Nanosecond()),
		}
	}

	// Mtime
	var setMtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setMtime); err != nil {
		return nil, fmt.Errorf("read set_mtime: %w", err)
	}
	if setMtime == 1 {
		newAttr.SetMtime = true
		if err := binary.Read(reader, binary.BigEndian, &newAttr.Mtime.Seconds); err != nil {
			return nil, fmt.Errorf("read mtime seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &newAttr.Mtime.Nseconds); err != nil {
			return nil, fmt.Errorf("read mtime nseconds: %w", err)
		}
	} else if setMtime == 2 {
		// SET_TO_SERVER_TIME
		newAttr.SetMtime = true
		now := time.Now()
		newAttr.Mtime = TimeVal{
			Seconds:  uint32(now.Unix()),
			Nseconds: uint32(now.Nanosecond()),
		}
	}

	// Guard
	guard := TimeGuard{}
	var guardCheck uint32
	if err := binary.Read(reader, binary.BigEndian, &guardCheck); err != nil {
		return nil, fmt.Errorf("read guard check: %w", err)
	}
	guard.Check = (guardCheck == 1)
	if guard.Check {
		if err := binary.Read(reader, binary.BigEndian, &guard.Time.Seconds); err != nil {
			return nil, fmt.Errorf("read guard time seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &guard.Time.Nseconds); err != nil {
			return nil, fmt.Errorf("read guard time nseconds: %w", err)
		}
	}

	return &SetAttrRequest{
		Handle:  handle,
		NewAttr: newAttr,
		Guard:   guard,
	}, nil
}

func (resp *SetAttrResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Write WCC data (pre-op and post-op attributes)
	// Pre-op attributes
	if resp.AttrBefore != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Size); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Seconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Nseconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Seconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Nseconds); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	// Post-op attributes
	if resp.AttrAfter != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.AttrAfter); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
