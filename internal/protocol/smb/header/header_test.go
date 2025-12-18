package header

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		want    *SMB2Header
		wantErr error
	}{
		{
			name:    "TooShort",
			data:    make([]byte, HeaderSize-1),
			want:    nil,
			wantErr: ErrMessageTooShort,
		},
		{
			name: "InvalidProtocolID",
			data: func() []byte {
				d := make([]byte, HeaderSize)
				d[0] = 0xFF // SMB1 protocol ID
				d[1] = 'S'
				d[2] = 'M'
				d[3] = 'B'
				return d
			}(),
			want:    nil,
			wantErr: ErrInvalidProtocolID,
		},
		{
			name: "InvalidStructureSize",
			data: func() []byte {
				d := make([]byte, HeaderSize)
				// Valid SMB2 protocol ID
				d[0] = 0xFE
				d[1] = 'S'
				d[2] = 'M'
				d[3] = 'B'
				// Invalid structure size
				d[4] = 0x00
				d[5] = 0x00
				return d
			}(),
			want:    nil,
			wantErr: ErrInvalidHeaderSize,
		},
		{
			name: "ValidNegotiateRequest",
			data: func() []byte {
				d := make([]byte, HeaderSize)
				// Protocol ID
				d[0] = 0xFE
				d[1] = 'S'
				d[2] = 'M'
				d[3] = 'B'
				// Structure size (64)
				d[4] = 0x40
				d[5] = 0x00
				// Credit charge
				d[6] = 0x01
				d[7] = 0x00
				// Status
				d[8] = 0x00
				d[9] = 0x00
				d[10] = 0x00
				d[11] = 0x00
				// Command (NEGOTIATE = 0)
				d[12] = 0x00
				d[13] = 0x00
				// Credits
				d[14] = 0x1F
				d[15] = 0x00
				// Flags
				d[16] = 0x00
				d[17] = 0x00
				d[18] = 0x00
				d[19] = 0x00
				// NextCommand
				d[20] = 0x00
				d[21] = 0x00
				d[22] = 0x00
				d[23] = 0x00
				// MessageID
				d[24] = 0x01
				d[25] = 0x00
				d[26] = 0x00
				d[27] = 0x00
				d[28] = 0x00
				d[29] = 0x00
				d[30] = 0x00
				d[31] = 0x00
				// Reserved
				d[32] = 0x00
				d[33] = 0x00
				d[34] = 0x00
				d[35] = 0x00
				// TreeID
				d[36] = 0x00
				d[37] = 0x00
				d[38] = 0x00
				d[39] = 0x00
				// SessionID
				d[40] = 0x00
				d[41] = 0x00
				d[42] = 0x00
				d[43] = 0x00
				d[44] = 0x00
				d[45] = 0x00
				d[46] = 0x00
				d[47] = 0x00
				// Signature (zeros)
				return d
			}(),
			want: &SMB2Header{
				ProtocolID:    [4]byte{0xFE, 'S', 'M', 'B'},
				StructureSize: 64,
				CreditCharge:  1,
				Status:        types.StatusSuccess,
				Command:       types.CommandNegotiate,
				Credits:       31,
				Flags:         0,
				NextCommand:   0,
				MessageID:     1,
				Reserved:      0,
				TreeID:        0,
				SessionID:     0,
				Signature:     [16]byte{},
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.data)

			if err != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.want == nil {
				if got != nil {
					t.Errorf("Parse() = %v, want nil", got)
				}
				return
			}

			if got.StructureSize != tt.want.StructureSize {
				t.Errorf("StructureSize = %d, want %d", got.StructureSize, tt.want.StructureSize)
			}
			if got.CreditCharge != tt.want.CreditCharge {
				t.Errorf("CreditCharge = %d, want %d", got.CreditCharge, tt.want.CreditCharge)
			}
			if got.Status != tt.want.Status {
				t.Errorf("Status = %v, want %v", got.Status, tt.want.Status)
			}
			if got.Command != tt.want.Command {
				t.Errorf("Command = %v, want %v", got.Command, tt.want.Command)
			}
			if got.Credits != tt.want.Credits {
				t.Errorf("Credits = %d, want %d", got.Credits, tt.want.Credits)
			}
			if got.MessageID != tt.want.MessageID {
				t.Errorf("MessageID = %d, want %d", got.MessageID, tt.want.MessageID)
			}
		})
	}
}

func TestSMB2Header_Encode(t *testing.T) {
	h := &SMB2Header{
		StructureSize: 64,
		CreditCharge:  1,
		Status:        types.StatusSuccess,
		Command:       types.CommandNegotiate,
		Credits:       256,
		Flags:         types.FlagResponse,
		NextCommand:   0,
		MessageID:     42,
		Reserved:      0,
		TreeID:        0,
		SessionID:     12345,
		Signature:     [16]byte{},
	}

	encoded := h.Encode()

	if len(encoded) != HeaderSize {
		t.Errorf("Encoded length = %d, want %d", len(encoded), HeaderSize)
	}

	// Parse back
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Failed to parse encoded header: %v", err)
	}

	if parsed.CreditCharge != h.CreditCharge {
		t.Errorf("CreditCharge round-trip: got %d, want %d", parsed.CreditCharge, h.CreditCharge)
	}
	if parsed.Status != h.Status {
		t.Errorf("Status round-trip: got %v, want %v", parsed.Status, h.Status)
	}
	if parsed.Command != h.Command {
		t.Errorf("Command round-trip: got %v, want %v", parsed.Command, h.Command)
	}
	if parsed.Credits != h.Credits {
		t.Errorf("Credits round-trip: got %d, want %d", parsed.Credits, h.Credits)
	}
	if parsed.Flags != h.Flags {
		t.Errorf("Flags round-trip: got %v, want %v", parsed.Flags, h.Flags)
	}
	if parsed.MessageID != h.MessageID {
		t.Errorf("MessageID round-trip: got %d, want %d", parsed.MessageID, h.MessageID)
	}
	if parsed.SessionID != h.SessionID {
		t.Errorf("SessionID round-trip: got %d, want %d", parsed.SessionID, h.SessionID)
	}
}

func TestNewResponseHeader(t *testing.T) {
	req := &SMB2Header{
		StructureSize: 64,
		CreditCharge:  1,
		Status:        types.StatusSuccess,
		Command:       types.CommandRead,
		Credits:       10,
		Flags:         0,
		NextCommand:   0,
		MessageID:     100,
		Reserved:      0,
		TreeID:        5,
		SessionID:     12345,
	}

	resp := NewResponseHeader(req, types.StatusSuccess)

	t.Run("CopiesRequestFields", func(t *testing.T) {
		if resp.Command != req.Command {
			t.Errorf("Command not copied: got %v, want %v", resp.Command, req.Command)
		}
		if resp.MessageID != req.MessageID {
			t.Errorf("MessageID not copied: got %d, want %d", resp.MessageID, req.MessageID)
		}
		if resp.TreeID != req.TreeID {
			t.Errorf("TreeID not copied: got %d, want %d", resp.TreeID, req.TreeID)
		}
		if resp.SessionID != req.SessionID {
			t.Errorf("SessionID not copied: got %d, want %d", resp.SessionID, req.SessionID)
		}
	})

	t.Run("SetsFlagResponse", func(t *testing.T) {
		if resp.Flags&types.FlagResponse == 0 {
			t.Errorf("FlagResponse not set: flags = %v", resp.Flags)
		}
	})

	t.Run("GrantsMinimumCredits", func(t *testing.T) {
		if resp.Credits < 256 {
			t.Errorf("Credits too low: got %d, want >= 256", resp.Credits)
		}
	})

	t.Run("SetsStatus", func(t *testing.T) {
		if resp.Status != types.StatusSuccess {
			t.Errorf("Status not set: got %v, want %v", resp.Status, types.StatusSuccess)
		}
	})
}

func TestNewResponseHeaderWithCredits(t *testing.T) {
	req := &SMB2Header{
		Command:   types.CommandWrite,
		Credits:   100,
		MessageID: 50,
		SessionID: 999,
	}

	customCredits := uint16(42)
	resp := NewResponseHeaderWithCredits(req, types.StatusMoreProcessingRequired, customCredits)

	if resp.Credits != customCredits {
		t.Errorf("Custom credits not set: got %d, want %d", resp.Credits, customCredits)
	}

	if resp.Status != types.StatusMoreProcessingRequired {
		t.Errorf("Status not set: got %v, want %v", resp.Status, types.StatusMoreProcessingRequired)
	}
}

func TestIsSMB2Message(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "ValidSMB2",
			data: []byte{0xFE, 'S', 'M', 'B', 0x40, 0x00},
			want: true,
		},
		{
			name: "SMB1",
			data: []byte{0xFF, 'S', 'M', 'B', 0x00, 0x00},
			want: false,
		},
		{
			name: "TooShort",
			data: []byte{0xFE, 'S', 'M'},
			want: false,
		},
		{
			name: "Empty",
			data: []byte{},
			want: false,
		},
		{
			name: "InvalidProtocol",
			data: []byte{0x00, 0x00, 0x00, 0x00},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSMB2Message(tt.data); got != tt.want {
				t.Errorf("IsSMB2Message() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSMB1Message(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "ValidSMB1",
			data: []byte{0xFF, 'S', 'M', 'B', 0x00, 0x00},
			want: true,
		},
		{
			name: "SMB2",
			data: []byte{0xFE, 'S', 'M', 'B', 0x40, 0x00},
			want: false,
		},
		{
			name: "TooShort",
			data: []byte{0xFF, 'S', 'M'},
			want: false,
		},
		{
			name: "Empty",
			data: []byte{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSMB1Message(tt.data); got != tt.want {
				t.Errorf("IsSMB1Message() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSMB2Header_Flags(t *testing.T) {
	t.Run("IsResponse", func(t *testing.T) {
		h := &SMB2Header{Flags: types.FlagResponse}
		if !h.IsResponse() {
			t.Error("IsResponse() should return true when FlagResponse is set")
		}

		h2 := &SMB2Header{Flags: 0}
		if h2.IsResponse() {
			t.Error("IsResponse() should return false when FlagResponse is not set")
		}
	})

	t.Run("IsSigned", func(t *testing.T) {
		h := &SMB2Header{Flags: types.FlagSigned}
		if !h.IsSigned() {
			t.Error("IsSigned() should return true when FlagSigned is set")
		}
	})

	t.Run("IsRelated", func(t *testing.T) {
		h := &SMB2Header{Flags: types.FlagRelated}
		if !h.IsRelated() {
			t.Error("IsRelated() should return true when FlagRelated is set")
		}
	})

	t.Run("IsAsync", func(t *testing.T) {
		h := &SMB2Header{Flags: types.FlagAsync}
		if !h.IsAsync() {
			t.Error("IsAsync() should return true when FlagAsync is set")
		}
	})
}

func TestSMB2Header_CommandName(t *testing.T) {
	tests := []struct {
		command types.Command
		want    string
	}{
		{types.CommandNegotiate, "NEGOTIATE"},
		{types.CommandSessionSetup, "SESSION_SETUP"},
		{types.CommandLogoff, "LOGOFF"},
		{types.CommandTreeConnect, "TREE_CONNECT"},
		{types.CommandTreeDisconnect, "TREE_DISCONNECT"},
		{types.CommandCreate, "CREATE"},
		{types.CommandClose, "CLOSE"},
		{types.CommandFlush, "FLUSH"},
		{types.CommandRead, "READ"},
		{types.CommandWrite, "WRITE"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			h := &SMB2Header{Command: tt.command}
			if got := h.CommandName(); got != tt.want {
				t.Errorf("CommandName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSMB2Header_StatusName(t *testing.T) {
	tests := []struct {
		status types.Status
		want   string
	}{
		{types.StatusSuccess, "STATUS_SUCCESS"},
		{types.StatusMoreProcessingRequired, "STATUS_MORE_PROCESSING_REQUIRED"},
		{types.StatusAccessDenied, "STATUS_ACCESS_DENIED"},
		{types.StatusLogonFailure, "STATUS_LOGON_FAILURE"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			h := &SMB2Header{Status: tt.status}
			if got := h.StatusName(); got != tt.want {
				t.Errorf("StatusName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// Test various command types
	commands := []types.Command{
		types.CommandNegotiate,
		types.CommandSessionSetup,
		types.CommandCreate,
		types.CommandRead,
		types.CommandWrite,
		types.CommandClose,
	}

	for _, cmd := range commands {
		t.Run(cmd.String(), func(t *testing.T) {
			original := &SMB2Header{
				StructureSize: HeaderSize,
				CreditCharge:  5,
				Status:        types.StatusMoreProcessingRequired,
				Command:       cmd,
				Credits:       512,
				Flags:         types.FlagResponse | types.FlagSigned,
				NextCommand:   128,
				MessageID:     999999,
				Reserved:      12345,
				TreeID:        42,
				SessionID:     0xDEADBEEF,
				Signature:     [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			}

			encoded := original.Encode()
			decoded, err := Parse(encoded)
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}

			if decoded.StructureSize != original.StructureSize {
				t.Errorf("StructureSize mismatch")
			}
			if decoded.CreditCharge != original.CreditCharge {
				t.Errorf("CreditCharge mismatch")
			}
			if decoded.Status != original.Status {
				t.Errorf("Status mismatch: got %v, want %v", decoded.Status, original.Status)
			}
			if decoded.Command != original.Command {
				t.Errorf("Command mismatch: got %v, want %v", decoded.Command, original.Command)
			}
			if decoded.Credits != original.Credits {
				t.Errorf("Credits mismatch")
			}
			if decoded.Flags != original.Flags {
				t.Errorf("Flags mismatch")
			}
			if decoded.NextCommand != original.NextCommand {
				t.Errorf("NextCommand mismatch")
			}
			if decoded.MessageID != original.MessageID {
				t.Errorf("MessageID mismatch")
			}
			if decoded.TreeID != original.TreeID {
				t.Errorf("TreeID mismatch")
			}
			if decoded.SessionID != original.SessionID {
				t.Errorf("SessionID mismatch")
			}
			if !bytes.Equal(decoded.Signature[:], original.Signature[:]) {
				t.Errorf("Signature mismatch")
			}
		})
	}
}
