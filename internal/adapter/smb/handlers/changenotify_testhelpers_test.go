package handlers

import (
	"testing"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/changenotify"
)

// newTestNotifyRegistry returns a registry whose flush timer will not fire
// during a test, so a test drives delivery itself and asserts on the buffer
// instead of racing the accumulation window.
func newTestNotifyRegistry() *changenotify.NotifyRegistry {
	return changenotify.NewNotifyRegistryWithFlushDelay(time.Hour)
}

// mustRegister arms a watch and fails the test if the registry refuses it, so a
// test that means to exercise delivery cannot silently exercise an empty
// registry instead.
func mustRegister(t *testing.T, r *changenotify.NotifyRegistry, n *changenotify.PendingNotify) {
	t.Helper()
	if err := r.Register(n); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
}

// encodeChangeNotifyReq builds an SMB2 CHANGE_NOTIFY request body
// per MS-SMB2 2.2.35.
func encodeChangeNotifyReq(flags uint16, outBufLen uint32, fileID [16]byte, completionFilter uint32) []byte {
	body := make([]byte, 32)
	// StructureSize = 32
	body[0] = 0x20
	body[1] = 0x00
	// Flags
	body[2] = byte(flags)
	body[3] = byte(flags >> 8)
	// OutputBufferLength
	body[4] = byte(outBufLen)
	body[5] = byte(outBufLen >> 8)
	body[6] = byte(outBufLen >> 16)
	body[7] = byte(outBufLen >> 24)
	// FileID
	copy(body[8:24], fileID[:])
	// CompletionFilter
	body[24] = byte(completionFilter)
	body[25] = byte(completionFilter >> 8)
	body[26] = byte(completionFilter >> 16)
	body[27] = byte(completionFilter >> 24)
	return body
}

// decodeFileNotifyInfos walks a FILE_NOTIFY_INFORMATION list (MS-FSCC §2.7.1 (FILE_NOTIFY_INFORMATION)).
// Test helper only — production decode happens client-side.
func decodeFileNotifyInfos(buf []byte) []changenotify.FileNotifyInformation {
	var out []changenotify.FileNotifyInformation
	off := 0
	for off+12 <= len(buf) {
		next := uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16 | uint32(buf[off+3])<<24
		action := uint32(buf[off+4]) | uint32(buf[off+5])<<8 | uint32(buf[off+6])<<16 | uint32(buf[off+7])<<24
		nameLen := uint32(buf[off+8]) | uint32(buf[off+9])<<8 | uint32(buf[off+10])<<16 | uint32(buf[off+11])<<24
		if off+12+int(nameLen) > len(buf) {
			break
		}
		u16 := make([]uint16, nameLen/2)
		for i := range u16 {
			u16[i] = uint16(buf[off+12+i*2]) | uint16(buf[off+12+i*2+1])<<8
		}
		out = append(out, changenotify.FileNotifyInformation{Action: action, FileName: string(utf16.Decode(u16))})
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out
}
