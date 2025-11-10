package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// CommitRequest represents a COMMIT request
type CommitRequest struct {
	Handle []byte
	Offset uint64
	Count  uint32
}

// CommitResponse represents a COMMIT response
type CommitResponse struct {
	Status        uint32
	AttrBefore    *types.WccAttr     // Pre-op attributes (optional)
	AttrAfter     *types.NFSFileAttr // Post-op attributes (optional)
	WriteVerifier uint64             // Write verifier (for detecting server reboots)
}

// Commit commits cached data on the server to stable storage.
// RFC 1813 Section 3.3.21
func (h *DefaultNFSHandler) Commit(repository metadata.Repository, req *CommitRequest) (*CommitResponse, error) {
	logger.Debug("COMMIT for handle: %x, offset=%d, count=%d", req.Handle, req.Offset, req.Count)

	// Get file attributes
	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Warn("File not found: %v", err)
		return &CommitResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Store pre-op attributes for WCC
	wccAttr := &types.WccAttr{
		Size: attr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	// In our simple implementation, we don't have write caching,
	// so COMMIT is essentially a no-op that always succeeds.
	// A real implementation would flush any pending writes to stable storage.

	logger.Info("COMMIT successful (no-op in current implementation)")

	return &CommitResponse{
		Status:        types.NFS3OK,
		AttrBefore:    wccAttr,
		AttrAfter:     nfsAttr,
		WriteVerifier: 0, // Static verifier since we don't track server reboots
	}, nil
}

func DecodeCommitRequest(data []byte) (*CommitRequest, error) {
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

	// Read offset
	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("read offset: %w", err)
	}

	// Read count
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read count: %w", err)
	}

	return &CommitRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

func (resp *CommitResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	if resp.Status != types.NFS3OK {
		// Write WCC data even on failure
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
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}

		return buf.Bytes(), nil
	}

	// Write WCC data
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
		if err := xdr.EncodeFileAttr(&buf, resp.AttrAfter); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	// Write verifier
	if err := binary.Write(&buf, binary.BigEndian, resp.WriteVerifier); err != nil {
		return nil, fmt.Errorf("write verifier: %w", err)
	}

	return buf.Bytes(), nil
}
