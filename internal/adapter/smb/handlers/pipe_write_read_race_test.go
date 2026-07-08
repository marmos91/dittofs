package handlers

// Regression test for #1607: the lost-wakeup race between the async-parking
// pipe READ path (handlePipeRead) and the write-side completion path
// (handlePipeWrite). Because every SMB2 request is dispatched in its own
// goroutine, a client's WRITE (carrying the LsarLookupSids2 PDU) and its READ
// (waiting for the response) run concurrently. Before the fix, the RPC response
// could be written into the pipe buffer while the parked READ registered a
// moment later — with nobody left to wake it — hanging Explorer's Security tab.
//
// This drives handlePipeWrite and handlePipeRead concurrently on the same pipe
// handle for many iterations under -race, asserting the LookupSids2 response is
// delivered exactly once every time (synchronously or via the async callback),
// and never lost.

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// buildRaceBindPDU builds a minimal LSA bind PDU (mirrors the rpc package's
// internal test builder, which is not importable here).
func buildRaceBindPDU(callID uint32) []byte {
	buf := make([]byte, 72)
	buf[0] = 5 // version major
	buf[2] = rpc.PDUBind
	buf[3] = rpc.FlagFirstFrag | rpc.FlagLastFrag
	buf[4] = 0x10                                     // little-endian data rep
	binary.LittleEndian.PutUint16(buf[8:10], 72)      // frag length
	binary.LittleEndian.PutUint32(buf[12:16], callID) // call ID
	binary.LittleEndian.PutUint16(buf[16:18], 4280)   // max xmit
	binary.LittleEndian.PutUint16(buf[18:20], 4280)   // max recv
	buf[24] = 1                                       // num contexts
	binary.LittleEndian.PutUint16(buf[28:30], 0)      // context ID
	buf[30] = 1                                       // num transfer syntaxes
	copy(buf[32:48], rpc.LSAInterfaceUUID[:])
	copy(buf[52:68], rpc.NDRTransferSyntaxUUID[:])
	binary.LittleEndian.PutUint32(buf[68:72], 2) // transfer syntax version
	return buf
}

// buildRaceLookupSids2PDU builds an LsarLookupSids2 (opnum 57) request PDU for a
// single SID.
func buildRaceLookupSids2PDU(callID uint32, s *sid.SID) []byte {
	var stub bytes.Buffer
	stub.Write(make([]byte, 20)) // 20-byte policy handle
	_ = binary.Write(&stub, binary.LittleEndian, uint32(1))          // Count
	_ = binary.Write(&stub, binary.LittleEndian, uint32(0x00020000)) // array pointer
	_ = binary.Write(&stub, binary.LittleEndian, uint32(1))          // conformant max
	_ = binary.Write(&stub, binary.LittleEndian, uint32(0x00020004)) // SID pointer
	_ = binary.Write(&stub, binary.LittleEndian, uint32(s.SubAuthorityCount))
	sid.EncodeSID(&stub, s)
	stubData := stub.Bytes()

	fragLen := rpc.HeaderSize + 8 + len(stubData)
	buf := make([]byte, fragLen)
	buf[0] = 5
	buf[2] = rpc.PDURequest
	buf[3] = rpc.FlagFirstFrag | rpc.FlagLastFrag
	buf[4] = 0x10
	binary.LittleEndian.PutUint16(buf[8:10], uint16(fragLen))
	binary.LittleEndian.PutUint32(buf[12:16], callID)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(stubData))) // alloc hint
	binary.LittleEndian.PutUint16(buf[22:24], rpc.OpLsarLookupSids2)
	copy(buf[24:], stubData)
	return buf
}

func TestHandlePipe_WriteReadRace_DeliversResponseOnce(t *testing.T) {
	const iterations = 300

	h := NewHandler()
	h.PipeManager = rpc.NewPipeManager()
	h.PipeReadRegistry = NewPipeReadRegistry()

	for i := 0; i < iterations; i++ {
		var fileID [16]byte
		binary.LittleEndian.PutUint64(fileID[:8], uint64(i)+1)

		// Register and bind the pipe, then drain the bind_ack so the buffer is
		// empty before the racing LookupSids2 exchange.
		pipe := h.PipeManager.CreatePipe(fileID, "lsarpc")
		if err := pipe.ProcessWrite(buildRaceBindPDU(1)); err != nil {
			t.Fatalf("iter %d: bind: %v", i, err)
		}
		if ack := pipe.ProcessRead(65536); len(ack) == 0 {
			t.Fatalf("iter %d: no bind_ack buffered", i)
		}

		// Capture an async-callback delivery, if the READ parks.
		cbCh := make(chan []byte, 1)
		ctx := NewSMBHandlerContext(context.TODO(), "race-client", 1, 1, uint64(i))
		ctx.TryReserveAsync = func() bool { return true }
		ctx.ReleaseAsync = func() {}
		ctx.AsyncPipeReadCallback = func(_, _, _ uint64, status types.Status, data []byte) error {
			cbCh <- data
			return nil
		}

		writeReq := &WriteRequest{FileID: fileID, Data: buildRaceLookupSids2PDU(2, sid.WellKnownEveryone)}
		readReq := &ReadRequest{FileID: fileID, Length: 4096}
		writeOpen := &OpenFile{IsPipe: true, PipeName: "lsarpc"}
		readOpen := &OpenFile{IsPipe: true, PipeName: "lsarpc"}

		var wg sync.WaitGroup
		var readResp *ReadResponse
		var readErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := h.handlePipeWrite(ctx, writeReq, writeOpen); err != nil {
				t.Errorf("iter %d: handlePipeWrite: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			readResp, readErr = h.handlePipeRead(ctx, readReq, readOpen)
		}()
		wg.Wait()

		if readErr != nil {
			t.Fatalf("iter %d: handlePipeRead: %v", i, readErr)
		}

		switch readResp.Status {
		case types.StatusSuccess:
			// Delivered synchronously (either the normal ordering or the
			// lost-wakeup recheck reclaimed the response).
			if len(readResp.Data) == 0 {
				t.Fatalf("iter %d: synchronous READ success carried no data", i)
			}
		case types.StatusPending:
			// Parked: the WRITE-side completion must have delivered via callback.
			select {
			case data := <-cbCh:
				if len(data) == 0 {
					t.Fatalf("iter %d: async callback delivered empty data", i)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("iter %d: READ parked (STATUS_PENDING) but response was never delivered — lost wakeup", i)
			}
		default:
			t.Fatalf("iter %d: unexpected READ status %v", i, readResp.Status)
		}

		h.PipeManager.ClosePipe(fileID)
	}
}
