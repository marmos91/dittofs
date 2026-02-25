// Package state -- shared wire-format helpers for NFSv4 callback encoding.
//
// These helpers are used by both v4.0 dial-out callbacks (callback.go) and
// v4.1 backchannel multiplexed callbacks (backchannel.go).

package state

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// Constants
// ============================================================================

const (
	// CbRPCVersion is the RPC version per RFC 5531.
	CbRPCVersion = 2

	// MaxFragmentSize is the maximum allowed RPC fragment size (1MB).
	MaxFragmentSize = 1 * 1024 * 1024
)

// ============================================================================
// Wire-Format Helpers
// ============================================================================

// EncodeCBRecallOp encodes one nfs_cb_argop4 for CB_RECALL.
//
// Wire format per RFC 7530 Section 16.2.2:
//
//	uint32    argop = OP_CB_RECALL (4)
//	stateid4  stateid
//	bool      truncate
//	nfs_fh4   fh
func EncodeCBRecallOp(stateid *types.Stateid4, truncate bool, fh []byte) []byte {
	var buf bytes.Buffer

	// argop: OP_CB_RECALL = 4
	_ = xdr.WriteUint32(&buf, types.OP_CB_RECALL)

	// stateid4
	types.EncodeStateid4(&buf, stateid)

	// truncate: bool as uint32
	_ = xdr.WriteBool(&buf, truncate)

	// fh: nfs_fh4 as XDR opaque
	_ = xdr.WriteXDROpaque(&buf, fh)

	return buf.Bytes()
}

// BuildCBRPCCallMessage builds an RPC CALL message with AUTH_NULL credentials.
//
// Wire format per RFC 5531:
//
//	XID:        [uint32]
//	MsgType:    [uint32] = 0 (CALL)
//	RPCVersion: [uint32] = 2
//	Program:    [uint32]
//	Version:    [uint32]
//	Procedure:  [uint32]
//	Cred:       AUTH_NULL (flavor=0, length=0)
//	Verf:       AUTH_NULL (flavor=0, length=0)
//	Args:       [procedure args]
func BuildCBRPCCallMessage(xid, prog, vers, proc uint32, args []byte) []byte {
	var buf bytes.Buffer

	// RPC header fields (writes to bytes.Buffer never fail)
	_ = xdr.WriteUint32(&buf, xid)
	_ = xdr.WriteUint32(&buf, uint32(rpc.RPCCall))
	_ = xdr.WriteUint32(&buf, uint32(CbRPCVersion))
	_ = xdr.WriteUint32(&buf, prog)
	_ = xdr.WriteUint32(&buf, vers)
	_ = xdr.WriteUint32(&buf, proc)

	// Auth credentials: AUTH_NULL (flavor=0, length=0)
	_ = xdr.WriteUint32(&buf, uint32(rpc.AuthNull))
	_ = xdr.WriteUint32(&buf, 0)

	// Auth verifier: AUTH_NULL (flavor=0, length=0)
	_ = xdr.WriteUint32(&buf, uint32(rpc.AuthNull))
	_ = xdr.WriteUint32(&buf, 0)

	// Procedure arguments
	_, _ = buf.Write(args)

	return buf.Bytes()
}

// AddCBRecordMark adds RPC record marking (fragment header) to a message.
//
// Per RFC 5531 Section 11 (Record Marking):
// For TCP, RPC messages are preceded by a 4-byte header:
//   - bit 31: Last fragment flag (1 = last fragment)
//   - bits 0-30: Fragment length
//
// Since we send the entire message as a single fragment, we set the
// last fragment bit.
func AddCBRecordMark(msg []byte, lastFragment bool) []byte {
	header := uint32(len(msg))
	if lastFragment {
		header |= 0x80000000
	}

	result := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(result[0:4], header)
	copy(result[4:], msg)

	return result
}

// ReadFragment reads an RPC record-marked fragment from a reader.
// It reads the 4-byte fragment header, validates the length, and returns
// the fragment body.
func ReadFragment(r io.Reader) ([]byte, error) {
	var headerBuf [4]byte
	if _, err := io.ReadFull(r, headerBuf[:]); err != nil {
		return nil, fmt.Errorf("read reply fragment header: %w", err)
	}

	header := binary.BigEndian.Uint32(headerBuf[:])
	fragLen := header & 0x7FFFFFFF

	if fragLen > MaxFragmentSize {
		return nil, fmt.Errorf("reply fragment too large: %d", fragLen)
	}

	body := make([]byte, fragLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read reply body: %w", err)
	}

	return body, nil
}

// EncodeCBNotifyOp encodes one nfs_cb_argop4 for CB_NOTIFY.
//
// Wire format per RFC 8881 Section 20.4:
//
//	uint32    argop = OP_CB_NOTIFY (6)
//	stateid4  stateid
//	nfs_fh4   fh
//	notify4   changes<>
//
// Notifications are grouped by type into Notify4 entries. Each Notify4
// carries a bitmap indicating the notification type and an array of
// opaque entries encoded using the appropriate sub-type encoder.
func EncodeCBNotifyOp(stateid *types.Stateid4, dirFH []byte, notifs []DirNotification, mask uint32) []byte {
	var buf bytes.Buffer

	// argop: CB_NOTIFY = 6
	_ = xdr.WriteUint32(&buf, types.CB_NOTIFY)

	// Build CbNotifyArgs using types
	args := types.CbNotifyArgs{
		Stateid: *stateid,
		FH:      dirFH,
	}

	// Group notifications by type
	grouped := make(map[uint32][]DirNotification)
	for _, n := range notifs {
		grouped[n.Type] = append(grouped[n.Type], n)
	}

	// Sort notification types for deterministic CB_NOTIFY encoding
	notifTypes := make([]uint32, 0, len(grouped))
	for t := range grouped {
		notifTypes = append(notifTypes, t)
	}
	slices.SortFunc(notifTypes, cmp.Compare)

	// Encode each group as a Notify4
	for _, notifType := range notifTypes {
		entries := grouped[notifType]
		// Only include types the client subscribed to
		if mask&(1<<notifType) == 0 {
			continue
		}

		notify := types.Notify4{
			Mask: types.Bitmap4{1 << notifType},
		}

		for _, entry := range entries {
			var entryBuf bytes.Buffer
			switch notifType {
			case types.NOTIFY4_ADD_ENTRY:
				enc := &types.NotifyAdd4{
					EntryName: entry.EntryName,
					Cookie:    entry.Cookie,
					Attrs:     entry.Attrs,
				}
				_ = enc.Encode(&entryBuf)
			case types.NOTIFY4_REMOVE_ENTRY:
				enc := &types.NotifyRemove4{
					EntryName: entry.EntryName,
					Cookie:    entry.Cookie,
				}
				_ = enc.Encode(&entryBuf)
			case types.NOTIFY4_RENAME_ENTRY:
				enc := &types.NotifyRename4{
					OldEntryName: entry.EntryName,
					NewEntryName: entry.NewName,
				}
				_ = enc.Encode(&entryBuf)
			case types.NOTIFY4_CHANGE_CHILD_ATTRS, types.NOTIFY4_CHANGE_DIR_ATTRS:
				enc := &types.NotifyAttrChange4{
					EntryName: entry.EntryName,
					Cookie:    entry.Cookie,
					Attrs:     entry.Attrs,
				}
				_ = enc.Encode(&entryBuf)
			default:
				// Unknown notification type -- encode as raw opaque
				_, _ = entryBuf.Write([]byte(entry.EntryName))
			}

			notify.Values = append(notify.Values, types.NotifyEntry4{
				Data: entryBuf.Bytes(),
			})
		}

		args.Changes = append(args.Changes, notify)
	}

	_ = args.Encode(&buf)

	return buf.Bytes()
}

// ValidateCBReply validates an RPC reply message buffer.
//
// It parses the RPC reply status and for CB_COMPOUND responses, checks the
// nfsstat4 status code. Returns nil on success or an error describing the failure.
//
// Wire format of RPC reply:
//
//	XID:         [uint32]
//	MsgType:     [uint32] = 1 (REPLY)
//	reply_stat:  [uint32] = 0 (MSG_ACCEPTED)
//	verf_flavor: [uint32]
//	verf_body:   [opaque, typically 0 length]
//	accept_stat: [uint32] = 0 (SUCCESS)
//	... procedure-specific results ...
func ValidateCBReply(replyBuf []byte) error {
	if len(replyBuf) < 12 {
		return fmt.Errorf("reply fragment too small: %d", len(replyBuf))
	}

	reader := bytes.NewReader(replyBuf)

	// Skip XID (4 bytes)
	var xid uint32
	if err := binary.Read(reader, binary.BigEndian, &xid); err != nil {
		return fmt.Errorf("read reply xid: %w", err)
	}

	// MsgType (should be REPLY = 1)
	var msgType uint32
	if err := binary.Read(reader, binary.BigEndian, &msgType); err != nil {
		return fmt.Errorf("read reply msg type: %w", err)
	}
	if msgType != rpc.RPCReply {
		return fmt.Errorf("expected RPC reply (1), got %d", msgType)
	}

	// reply_stat (should be MSG_ACCEPTED = 0)
	var replyStat uint32
	if err := binary.Read(reader, binary.BigEndian, &replyStat); err != nil {
		return fmt.Errorf("read reply stat: %w", err)
	}
	if replyStat != rpc.RPCMsgAccepted {
		return fmt.Errorf("RPC call denied: reply_stat=%d", replyStat)
	}

	// Verifier: flavor + body length + body
	var verfFlavor uint32
	if err := binary.Read(reader, binary.BigEndian, &verfFlavor); err != nil {
		return fmt.Errorf("read verf flavor: %w", err)
	}
	var verfLen uint32
	if err := binary.Read(reader, binary.BigEndian, &verfLen); err != nil {
		return fmt.Errorf("read verf length: %w", err)
	}
	if verfLen > 0 {
		// XDR opaque fields are padded to 4-byte boundaries
		paddedLen := int64((verfLen + 3) &^ 3)
		if _, err := reader.Seek(paddedLen, io.SeekCurrent); err != nil {
			return fmt.Errorf("skip verf body: %w", err)
		}
	}

	// accept_stat (should be SUCCESS = 0)
	var acceptStat uint32
	if err := binary.Read(reader, binary.BigEndian, &acceptStat); err != nil {
		return fmt.Errorf("read accept stat: %w", err)
	}
	if acceptStat != rpc.RPCSuccess {
		return fmt.Errorf("RPC call not successful: accept_stat=%d", acceptStat)
	}

	// For CB_COMPOUND: parse nfsstat4 from the response body
	if reader.Len() >= 4 {
		var nfsStatus uint32
		if err := binary.Read(reader, binary.BigEndian, &nfsStatus); err != nil {
			return fmt.Errorf("read nfsstat4: %w", err)
		}
		if nfsStatus != types.NFS4_OK {
			return fmt.Errorf("CB_COMPOUND failed: nfsstat4=%d", nfsStatus)
		}
	}

	return nil
}
