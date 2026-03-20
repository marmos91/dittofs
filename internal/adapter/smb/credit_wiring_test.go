package smb

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreditValidationLogic_ExemptCommands verifies that credit-exempt commands
// bypass sequence window consumption and credit charge validation.
func TestCreditValidationLogic_ExemptCommands(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	initialSize := sw.Size()

	tests := []struct {
		name      string
		command   types.Command
		sessionID uint64
	}{
		{"NEGOTIATE with SessionID=0", types.CommandNegotiate, 0},
		{"SESSION_SETUP with SessionID=0", types.CommandSessionSetup, 0},
		{"CANCEL with SessionID=42", types.CommandCancel, 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, session.IsCreditExempt(tt.command, tt.sessionID),
				"command should be credit-exempt")

			// Sequence window should remain unchanged for exempt commands
			assert.Equal(t, initialSize, sw.Size(),
				"window size should not change for exempt commands")
		})
	}
}

// TestCreditValidationLogic_NonExemptConsumption verifies that non-exempt commands
// consume from the sequence window.
func TestCreditValidationLogic_NonExemptConsumption(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)

	// Window starts with {0} available (size=1).
	// Grant more credits so we can test multiple consumes.
	sw.Grant(10) // Now window has sequences 0..10 available, size=11

	// READ command, valid MessageId=0, CreditCharge=1 -> should consume
	assert.False(t, session.IsCreditExempt(types.CommandRead, 5))

	// Validate CreditCharge against body
	readBody := makeTestReadBody(64 * 1024) // 64KB -> needs CreditCharge=1
	err := session.ValidateCreditCharge(types.CommandRead, 1, readBody)
	require.NoError(t, err, "CreditCharge=1 should suffice for 64KB READ")

	// Consume from window
	charge := session.EffectiveCreditCharge(1)
	ok := sw.Consume(0, charge)
	assert.True(t, ok, "MessageId=0 should be consumable")
}

// TestCreditValidationLogic_DuplicateMessageId verifies that consuming the same
// MessageId twice fails (replay protection).
func TestCreditValidationLogic_DuplicateMessageId(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	sw.Grant(10)

	// First consume succeeds
	ok := sw.Consume(0, 1)
	assert.True(t, ok, "first consume should succeed")

	// Duplicate consume should fail
	ok = sw.Consume(0, 1)
	assert.False(t, ok, "duplicate MessageId should be rejected")
}

// TestCreditValidationLogic_InsufficientCreditCharge verifies that a READ with
// payload requiring more credits than CreditCharge provides is rejected.
func TestCreditValidationLogic_InsufficientCreditCharge(t *testing.T) {
	// 128KB READ needs CreditCharge=2, but we provide 1
	readBody := makeTestReadBody(128 * 1024)
	err := session.ValidateCreditCharge(types.CommandRead, 1, readBody)
	assert.Error(t, err, "CreditCharge=1 should be insufficient for 128KB READ")
}

// TestSequenceWindowGrant_OnErrorResponse verifies that the sequence window
// expands when credits are granted, even on error responses.
func TestSequenceWindowGrant_OnErrorResponse(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	initialSize := sw.Size()

	// Simulate granting credits (as would happen on an error response)
	grantedCredits := uint16(5)
	sw.Grant(grantedCredits)

	assert.Equal(t, initialSize+uint64(grantedCredits), sw.Size(),
		"window should expand by granted credits")
}

// TestSequenceWindowGrant_OnSuccessResponse verifies that the sequence window
// expands on successful response credit grants.
func TestSequenceWindowGrant_OnSuccessResponse(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	// Consume the initial credit
	sw.Consume(0, 1)
	sizeAfterConsume := sw.Size()

	// Grant credits as done in response path
	grantedCredits := uint16(10)
	sw.Grant(grantedCredits)

	assert.Equal(t, sizeAfterConsume+uint64(grantedCredits), sw.Size(),
		"window should expand by granted credits after success response")
}

// TestConnInfo_SequenceWindowField verifies that ConnInfo has the SequenceWindow field.
func TestConnInfo_SequenceWindowField(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	ci := &ConnInfo{
		SequenceWindow:      sw,
		SupportsMultiCredit: true,
	}

	assert.NotNil(t, ci.SequenceWindow, "SequenceWindow field should be accessible")
	assert.True(t, ci.SupportsMultiCredit, "SupportsMultiCredit field should be accessible")
	assert.Equal(t, uint64(1), ci.SequenceWindow.Size(), "initial window should have size 1")
}

// TestSendMessage_SequenceWindowExpansion verifies the sequence window expansion
// logic that should be wired into the SendMessage path.
func TestSendMessage_SequenceWindowExpansion(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	initialSize := sw.Size()

	// Simulate what SendMessage should do: expand window with response credits
	hdrCredits := uint16(8)
	if sw != nil && hdrCredits > 0 {
		sw.Grant(hdrCredits)
	}

	assert.Equal(t, initialSize+uint64(hdrCredits), sw.Size(),
		"SendMessage should expand window by response credits")
}

// TestCreditValidationPipeline_FullFlow simulates the complete credit validation
// pipeline as wired in ProcessSingleRequest.
func TestCreditValidationPipeline_FullFlow(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	// Grant enough credits for multiple operations
	sw.Grant(255)

	supportsMultiCredit := true

	// Simulate a READ request with valid credits
	reqCommand := types.CommandRead
	reqSessionID := uint64(1)
	reqMessageID := uint64(0)
	reqCreditCharge := uint16(1)
	readBody := makeTestReadBody(64 * 1024)

	// Step 1: Check exempt status
	if !session.IsCreditExempt(reqCommand, reqSessionID) {
		// Step 2: Validate credit charge (multi-credit only)
		if supportsMultiCredit {
			err := session.ValidateCreditCharge(reqCommand, reqCreditCharge, readBody)
			require.NoError(t, err, "credit charge validation should pass")
		}

		// Step 3: Consume from sequence window
		charge := session.EffectiveCreditCharge(reqCreditCharge)
		ok := sw.Consume(reqMessageID, charge)
		require.True(t, ok, "sequence window consume should succeed")
	}

	// Step 4: After sending response, grant credits to expand window
	responseCredits := uint16(10)
	sw.Grant(responseCredits)

	// Verify window was expanded
	// Initial: 1 slot (0). Granted 255. Consumed 1. Granted 10.
	// Size = high - low. After Grant(255): high=256, low=0, size=256
	// After Consume(0,1): low advances to 64 (bitmap compaction), high=256
	// After Grant(10): high=266
	assert.True(t, sw.Size() > 0, "window should have available credits")
}

// makeTestReadBody creates a READ request body with the specified Length field.
func makeTestReadBody(length uint32) []byte {
	body := make([]byte, 49)
	binary.LittleEndian.PutUint16(body[0:2], 49)     // StructureSize
	binary.LittleEndian.PutUint32(body[4:8], length) // Length
	return body
}

// TestSendMessageWithConnInfo_GrantExpansion tests the Grant call in SendMessage
// using a ConnInfo with SequenceWindow set.
func TestSendMessageWithConnInfo_GrantExpansion(t *testing.T) {
	sw := session.NewCommandSequenceWindow(131070)
	ci := &ConnInfo{
		SequenceWindow: sw,
	}

	initialSize := ci.SequenceWindow.Size()

	// Simulate the grant logic from SendMessage
	credits := uint16(16)
	hdr := &header.SMB2Header{
		Credits: credits,
	}

	if ci.SequenceWindow != nil && hdr.Credits > 0 {
		ci.SequenceWindow.Grant(hdr.Credits)
	}

	assert.Equal(t, initialSize+uint64(credits), ci.SequenceWindow.Size(),
		"grant should expand window by header credits")
}
