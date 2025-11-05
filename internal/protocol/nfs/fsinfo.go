package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// FsInfoRequest represents a FSINFO request
type FsInfoRequest struct {
	Handle []byte
}

// FsInfoResponse represents a FSINFO response
type FsInfoResponse struct {
	Status      uint32
	Attr        *FileAttr // Post-op attributes (optional)
	Rtmax       uint32    // Max read transfer size
	Rtpref      uint32    // Preferred read transfer size
	Rtmult      uint32    // Suggested read multiple
	Wtmax       uint32    // Max write transfer size
	Wtpref      uint32    // Preferred write transfer size
	Wtmult      uint32    // Suggested write multiple
	Dtpref      uint32    // Preferred readdir transfer size
	Maxfilesize uint64    // Max file size
	TimeDelta   TimeVal   // Server time granularity
	Properties  uint32    // File system properties bitmap
}

// FsInfo returns static information about a file system.
// RFC 1813 Section 3.3.19
func (h *DefaultNFSHandler) FsInfo(repository metadata.Repository, req *FsInfoRequest) (*FsInfoResponse, error) {
	logger.Debug("FSINFO for handle: %x", req.Handle)

	// Look up the file in the repository (to verify it exists)
	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("File not found: %v", err)
		return &FsInfoResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Generate a file ID from the handle
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	// Return file system info
	// These values are typical for NFS servers
	resp := &FsInfoResponse{
		Status:      NFS3OK,
		Attr:        nfsAttr,
		Rtmax:       65536,     // 64KB max read
		Rtpref:      32768,     // 32KB preferred read
		Rtmult:      4096,      // 4KB read multiple
		Wtmax:       65536,     // 64KB max write
		Wtpref:      32768,     // 32KB preferred write
		Wtmult:      4096,      // 4KB write multiple
		Dtpref:      8192,      // 8KB preferred readdir
		Maxfilesize: 1<<63 - 1, // Max file size (practically unlimited)
		TimeDelta: TimeVal{
			Seconds:  0,
			Nseconds: 1, // 1 nanosecond time granularity
		},
		Properties: FSFLink | FSFSymlink | FSFHomogeneous | FSFCanSetTime,
	}

	logger.Debug("Returning FSINFO: rtmax=%d, wtmax=%d", resp.Rtmax, resp.Wtmax)

	return resp, nil
}

func DecodeFsInfoRequest(data []byte) (*FsInfoRequest, error) {
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

	return &FsInfoRequest{Handle: handle}, nil
}

func (resp *FsInfoResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, we're done
	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
	}

	// Write post-op attributes (present = true, then attributes)
	if resp.Attr != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil { // attributes_follow = TRUE
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil { // attributes_follow = FALSE
			return nil, err
		}
	}

	// Write fsinfo fields
	if err := binary.Write(&buf, binary.BigEndian, resp.Rtmax); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Rtpref); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Rtmult); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Wtmax); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Wtpref); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Wtmult); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Dtpref); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Maxfilesize); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.TimeDelta.Seconds); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.TimeDelta.Nseconds); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Properties); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
