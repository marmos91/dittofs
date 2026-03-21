package session

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/stretchr/testify/assert"
)

func TestIsCreditExempt(t *testing.T) {
	tests := []struct {
		name      string
		command   types.Command
		sessionID uint64
		want      bool
	}{
		{
			name:      "CANCEL is always exempt",
			command:   types.CommandCancel,
			sessionID: 42,
			want:      true,
		},
		{
			name:      "CANCEL exempt with sessionID 0",
			command:   types.CommandCancel,
			sessionID: 0,
			want:      true,
		},
		{
			name:      "NEGOTIATE with sessionID 0 is exempt",
			command:   types.CommandNegotiate,
			sessionID: 0,
			want:      true,
		},
		{
			name:      "SESSION_SETUP with sessionID 0 is exempt (first setup)",
			command:   types.CommandSessionSetup,
			sessionID: 0,
			want:      true,
		},
		{
			name:      "SESSION_SETUP with sessionID 5 is NOT exempt (re-auth)",
			command:   types.CommandSessionSetup,
			sessionID: 5,
			want:      false,
		},
		{
			name:      "READ with sessionID 5 is NOT exempt",
			command:   types.CommandRead,
			sessionID: 5,
			want:      false,
		},
		{
			name:      "WRITE with sessionID 99 is NOT exempt",
			command:   types.CommandWrite,
			sessionID: 99,
			want:      false,
		},
		{
			name:      "CLOSE with sessionID 1 is NOT exempt",
			command:   types.CommandClose,
			sessionID: 1,
			want:      false,
		},
		{
			name:      "NEGOTIATE with non-zero session is NOT exempt",
			command:   types.CommandNegotiate,
			sessionID: 1,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCreditExempt(tt.command, tt.sessionID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEffectiveCreditCharge(t *testing.T) {
	tests := []struct {
		name         string
		creditCharge uint16
		want         uint16
	}{
		{"zero treated as one", 0, 1},
		{"one stays one", 1, 1},
		{"three stays three", 3, 3},
		{"large value unchanged", 100, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveCreditCharge(tt.creditCharge)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateCreditCharge(t *testing.T) {
	// Helper to build a READ request body with Length field at offset 4
	makeReadBody := func(length uint32) []byte {
		body := make([]byte, 49)                         // SMB2 READ request is 49 bytes
		binary.LittleEndian.PutUint16(body[0:2], 49)     // StructureSize
		binary.LittleEndian.PutUint32(body[4:8], length) // Length
		return body
	}

	// Helper to build a WRITE request body with Length (DataLength) at offset 4
	makeWriteBody := func(dataLength uint32) []byte {
		body := make([]byte, 49)                             // SMB2 WRITE request is 49 bytes
		binary.LittleEndian.PutUint16(body[0:2], 49)         // StructureSize
		binary.LittleEndian.PutUint32(body[4:8], dataLength) // Length (data offset 70 in packet, but in body offset 4)
		return body
	}

	// Helper to build an IOCTL request body with MaxOutputResponse at offset 28
	makeIoctlBody := func(maxOutput uint32) []byte {
		body := make([]byte, 57)                              // SMB2 IOCTL request is 57 bytes
		binary.LittleEndian.PutUint16(body[0:2], 57)          // StructureSize
		binary.LittleEndian.PutUint32(body[28:32], maxOutput) // MaxOutputResponse
		return body
	}

	// Helper to build a QUERY_DIRECTORY body with OutputBufferLength at offset 4
	makeQueryDirBody := func(outputLen uint32) []byte {
		body := make([]byte, 33)                            // SMB2 QUERY_DIRECTORY request is 33 bytes
		binary.LittleEndian.PutUint16(body[0:2], 33)        // StructureSize
		binary.LittleEndian.PutUint32(body[4:8], outputLen) // OutputBufferLength
		return body
	}

	tests := []struct {
		name         string
		command      types.Command
		creditCharge uint16
		body         []byte
		wantErr      bool
	}{
		{
			name:         "READ 128KB with CreditCharge=1 should fail",
			command:      types.CommandRead,
			creditCharge: 1,
			body:         makeReadBody(128 * 1024),
			wantErr:      true,
		},
		{
			name:         "READ 128KB with CreditCharge=2 should pass",
			command:      types.CommandRead,
			creditCharge: 2,
			body:         makeReadBody(128 * 1024),
			wantErr:      false,
		},
		{
			name:         "READ 64KB with CreditCharge=1 should pass",
			command:      types.CommandRead,
			creditCharge: 1,
			body:         makeReadBody(64 * 1024),
			wantErr:      false,
		},
		{
			name:         "WRITE 65536 with CreditCharge=1 should pass",
			command:      types.CommandWrite,
			creditCharge: 1,
			body:         makeWriteBody(65536),
			wantErr:      false,
		},
		{
			name:         "WRITE 128KB with CreditCharge=1 should fail",
			command:      types.CommandWrite,
			creditCharge: 1,
			body:         makeWriteBody(128 * 1024),
			wantErr:      true,
		},
		{
			name:         "WRITE 128KB with CreditCharge=2 should pass",
			command:      types.CommandWrite,
			creditCharge: 2,
			body:         makeWriteBody(128 * 1024),
			wantErr:      false,
		},
		{
			name:         "IOCTL with large MaxOutputResponse and sufficient credits",
			command:      types.CommandIoctl,
			creditCharge: 3,
			body:         makeIoctlBody(192 * 1024),
			wantErr:      false,
		},
		{
			name:         "IOCTL with large MaxOutputResponse and insufficient credits",
			command:      types.CommandIoctl,
			creditCharge: 1,
			body:         makeIoctlBody(192 * 1024),
			wantErr:      true,
		},
		{
			name:         "QUERY_DIRECTORY with sufficient credits",
			command:      types.CommandQueryDirectory,
			creditCharge: 2,
			body:         makeQueryDirBody(128 * 1024),
			wantErr:      false,
		},
		{
			name:         "QUERY_DIRECTORY with insufficient credits",
			command:      types.CommandQueryDirectory,
			creditCharge: 1,
			body:         makeQueryDirBody(128 * 1024),
			wantErr:      true,
		},
		{
			name:         "CLOSE (non-payload command) always passes",
			command:      types.CommandClose,
			creditCharge: 1,
			body:         make([]byte, 24),
			wantErr:      false,
		},
		{
			name:         "CREATE (non-payload command) always passes",
			command:      types.CommandCreate,
			creditCharge: 1,
			body:         make([]byte, 57),
			wantErr:      false,
		},
		{
			name:         "READ with CreditCharge=0 allows up to 64KB",
			command:      types.CommandRead,
			creditCharge: 0,
			body:         makeReadBody(64 * 1024),
			wantErr:      false,
		},
		{
			name:         "READ with CreditCharge=0 rejects > 64KB",
			command:      types.CommandRead,
			creditCharge: 0,
			body:         makeReadBody(64*1024 + 1),
			wantErr:      true,
		},
		{
			name:         "READ with body too short returns nil (safe fallback)",
			command:      types.CommandRead,
			creditCharge: 1,
			body:         make([]byte, 4), // Too short to read Length at offset 4
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreditCharge(tt.command, tt.creditCharge, tt.body)
			if tt.wantErr {
				assert.Error(t, err, "expected error for %s", tt.name)
			} else {
				assert.NoError(t, err, "expected no error for %s", tt.name)
			}
		})
	}
}
