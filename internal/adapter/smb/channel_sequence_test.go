package smb

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func TestChannelSeqModify(t *testing.T) {
	modify := []types.Command{types.CommandWrite, types.CommandSetInfo, types.CommandIoctl}
	for _, c := range modify {
		if !channelSeqModify(c) {
			t.Errorf("command %s should be a modifying op", c)
		}
	}
	nonModify := []types.Command{
		types.CommandRead, types.CommandCreate, types.CommandClose,
		types.CommandFlush, types.CommandLock, types.CommandQueryInfo,
	}
	for _, c := range nonModify {
		if channelSeqModify(c) {
			t.Errorf("command %s should not be a modifying op", c)
		}
	}
}

func TestChannelSeqFileID(t *testing.T) {
	want := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	// READ/WRITE/SET_INFO place the FileId 16 bytes into the body.
	body16 := make([]byte, 64)
	copy(body16[16:32], want[:])
	for _, c := range []types.Command{types.CommandRead, types.CommandWrite, types.CommandSetInfo} {
		got, ok := channelSeqFileID(c, body16)
		if !ok || got != want {
			t.Errorf("%s: got (%v, %v), want (%v, true)", c, got, ok, want)
		}
	}

	// IOCTL places the FileId 8 bytes into the body.
	bodyIoctl := make([]byte, 64)
	copy(bodyIoctl[8:24], want[:])
	got, ok := channelSeqFileID(types.CommandIoctl, bodyIoctl)
	if !ok || got != want {
		t.Errorf("ioctl: got (%v, %v), want (%v, true)", got, ok, want)
	}

	// Commands without a fixed FileId offset are skipped.
	if _, ok := channelSeqFileID(types.CommandCreate, body16); ok {
		t.Error("CREATE should not yield a FileId for channel-sequence")
	}

	// Bodies too short to hold the FileId are rejected.
	if _, ok := channelSeqFileID(types.CommandWrite, make([]byte, 20)); ok {
		t.Error("short WRITE body should not yield a FileId")
	}
}
