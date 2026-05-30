package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// buildCreateRequestWire assembles a full SMB2 CREATE request BODY (the bytes
// after the 64-byte SMB2 header) for the given fields + create contexts, exactly
// as a real client would put them on the wire. It then runs DecodeCreateRequest
// over those bytes so the test exercises the real wire-decode path (the layer the
// reopen2_e2e_test.go harness bypasses by injecting []CreateContext directly).
func buildCreateRequestWire(
	oplockLevel uint8,
	desiredAccess, shareAccess, disposition, createOptions uint32,
	filename string,
	contexts []CreateContext,
) []byte {
	nameUTF16 := encodeUTF16LE(filename)

	// Fixed part is 56 bytes; name follows at body offset 56 (header+56 = 120).
	const fixedSize = 56
	nameOffsetFromHeader := uint16(64 + fixedSize) // 120

	// Encode contexts; they follow the (8-byte-aligned) name.
	ctxBuf, _, _ := EncodeCreateContexts(contexts)

	// Lay out: [56 fixed][name][pad to 8][contexts]
	body := make([]byte, fixedSize)
	binary.LittleEndian.PutUint16(body[0:2], 57) // StructureSize (always 57 for request)
	body[2] = 0                                  // SecurityFlags
	body[3] = oplockLevel                        // OplockLevel
	binary.LittleEndian.PutUint32(body[4:8], 0)  // ImpersonationLevel
	// SmbCreateFlags (8) + Reserved (8) at 8..24 = zero
	binary.LittleEndian.PutUint32(body[24:28], desiredAccess)
	binary.LittleEndian.PutUint32(body[28:32], 0) // FileAttributes
	binary.LittleEndian.PutUint32(body[32:36], shareAccess)
	binary.LittleEndian.PutUint32(body[36:40], disposition)
	binary.LittleEndian.PutUint32(body[40:44], createOptions)
	binary.LittleEndian.PutUint16(body[44:46], nameOffsetFromHeader)
	binary.LittleEndian.PutUint16(body[46:48], uint16(len(nameUTF16)))
	// CreateContextsOffset (48..52) + CreateContextsLength (52..56) filled below.

	// Append name.
	body = append(body, nameUTF16...)
	// Pad to 8-byte boundary before contexts.
	for len(body)%8 != 0 {
		body = append(body, 0)
	}

	if len(ctxBuf) > 0 {
		ctxOffsetFromHeader := uint32(64 + len(body))
		binary.LittleEndian.PutUint32(body[48:52], ctxOffsetFromHeader)
		binary.LittleEndian.PutUint32(body[52:56], uint32(len(ctxBuf)))
		body = append(body, ctxBuf...)
	}

	return body
}

// TestRepro_WireDecode_V1LeaseReconnect drives the FULL wire-decode path for the
// V1-lease reopen2 reconnect: build the on-wire CREATE body that smbtorture
// smb2.durable-open.reopen2-lease sends on the positive reconnect (DHnC + a V1
// 32-byte RqLs with the correct lease key + fname), decode it via
// DecodeCreateRequest, and assert the decoded contexts survive intact.
func TestRepro_WireDecode_V1LeaseReconnect(t *testing.T) {
	leaseKey := [16]byte{0xAB, 0xCD, 0xEF, 0x01}
	persistentFileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	rwh := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)

	contexts := []CreateContext{
		dhncContext(persistentFileID),
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, rwh)},
	}

	body := buildCreateRequestWire(
		OplockLevelLease,
		0x001F01FF, 0x07, 0x01 /* FILE_OPEN */, 0,
		"durable.txt",
		contexts,
	)

	req, err := DecodeCreateRequest(body)
	if err != nil {
		t.Fatalf("DecodeCreateRequest failed on V1-lease reconnect wire body: %v", err)
	}
	if req.FileName != "durable.txt" {
		t.Errorf("decoded FileName = %q, want durable.txt", req.FileName)
	}
	dhnc := FindCreateContext(req.CreateContexts, DurableHandleV1ReconnectTag)
	if dhnc == nil {
		t.Fatal("decoded request LOST the DHnC reconnect context")
	}
	gotFileID, derr := DecodeDHnCReconnect(dhnc.Data)
	if derr != nil {
		t.Fatalf("decode DHnC: %v", derr)
	}
	if gotFileID != persistentFileID {
		t.Errorf("decoded DHnC FileID = %x, want %x", gotFileID, persistentFileID)
	}
	rqls := FindCreateContext(req.CreateContexts, LeaseContextTagRequest)
	if rqls == nil {
		t.Fatal("decoded request LOST the V1 RqLs lease context")
	}
	lc, lerr := DecodeLeaseCreateContext(rqls.Data)
	if lerr != nil || lc == nil {
		t.Fatalf("decode V1 RqLs: %v", lerr)
	}
	if lc.LeaseKey != leaseKey {
		t.Errorf("decoded lease key = %x, want %x", lc.LeaseKey, leaseKey)
	}
	t.Logf("decoded %d contexts: DHnC fileID=%x, RqLs leaseKey=%x state=0x%x",
		len(req.CreateContexts), gotFileID, lc.LeaseKey, lc.LeaseState)
}
