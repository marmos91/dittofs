package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// PathConfRequest represents a PATHCONF request
type PathConfRequest struct {
	Handle []byte
}

// PathConfResponse represents a PATHCONF response
type PathConfResponse struct {
	Status          uint32
	Attr            *FileAttr // Post-op attributes (optional)
	Linkmax         uint32    // Max number of hard links
	NameMax         uint32    // Max filename length
	NoTrunc         bool      // Server rejects long names
	ChownRestricted bool      // chown is restricted to root
	CaseInsensitive bool      // Case insensitive filename comparison
	CasePreserving  bool      // Case preserving filename
}

// PathConf returns POSIX information about a file system object.
// RFC 1813 Section 3.3.20
func (h *DefaultNFSHandler) PathConf(repository metadata.Repository, req *PathConfRequest) (*PathConfResponse, error) {
	logger.Debug("PATHCONF for handle: %x", req.Handle)

	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("File not found: %v", err)
		return &PathConfResponse{Status: NFS3ErrNoEnt}, nil
	}

	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	resp := &PathConfResponse{
		Status:          NFS3OK,
		Attr:            nfsAttr,
		Linkmax:         255,   // Max hard links
		NameMax:         255,   // Max filename length
		NoTrunc:         true,  // Reject long names
		ChownRestricted: true,  // chown restricted to root
		CaseInsensitive: false, // Case sensitive
		CasePreserving:  true,  // Case preserving
	}

	logger.Debug("Returning PATHCONF")
	return resp, nil
}

func DecodePathConfRequest(data []byte) (*PathConfRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short")
	}

	reader := bytes.NewReader(data)

	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	return &PathConfRequest{Handle: handle}, nil
}

func (resp *PathConfResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
	}

	// Write post-op attributes
	if resp.Attr != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	if err := binary.Write(&buf, binary.BigEndian, resp.Linkmax); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.NameMax); err != nil {
		return nil, err
	}

	noTrunc := uint32(0)
	if resp.NoTrunc {
		noTrunc = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, noTrunc); err != nil {
		return nil, err
	}

	chownRestricted := uint32(0)
	if resp.ChownRestricted {
		chownRestricted = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, chownRestricted); err != nil {
		return nil, err
	}

	caseInsensitive := uint32(0)
	if resp.CaseInsensitive {
		caseInsensitive = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, caseInsensitive); err != nil {
		return nil, err
	}

	casePreserving := uint32(0)
	if resp.CasePreserving {
		casePreserving = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, casePreserving); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
