package types

import (
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TreeConnection represents an active tree connection mapping a client
// to a DittoFS share. Created by TreeConnect and removed by TreeDisconnect.
// Stores the effective permission level for access control during file operations.

type TreeConnection struct {
	TreeID      uint32
	SessionID   uint64
	ShareName   string
	ShareType   uint8
	CreatedAt   time.Time
	Permission  models.SharePermission // User's permission level for this share
	EncryptData bool                   // Share requires all requests to be encrypted
	// AccessBasedEnumeration mirrors the share-level toggle. When true,
	// QUERY_DIRECTORY filters entries the caller cannot read (refs #532,
	// MS-SMB2 §2.2.10 SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM).
	AccessBasedEnumeration bool
	// ChangeNotifyDisabled mirrors the share-level toggle. When true,
	// CHANGE_NOTIFY requests on this tree are rejected with
	// STATUS_NOT_IMPLEMENTED — matches Samba `kernel change notify = no`
	// and the smb2.change_notify_disabled torture test.
	ChangeNotifyDisabled bool
	// StreamsDisabled mirrors the share-level toggle. When true, CREATE
	// requests that reference an Alternate Data Stream are rejected with
	// STATUS_OBJECT_NAME_INVALID — matches Samba `smbd:streams = no`
	// and the smb2.create_no_streams.no_stream torture test.
	StreamsDisabled bool
	// ContinuousAvailability mirrors the share-level toggle. When true, the
	// TREE_CONNECT response advertises SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY
	// (MS-SMB2 §2.2.10) and a DH2Q SMB2_DHANDLE_FLAG_PERSISTENT request is
	// granted as a persistent durable handle (#739, smbtorture
	// smb2.durable-v2-open.persistent-open-{oplock,lease}).
	ContinuousAvailability bool
	// AllowMFsymlink mirrors the share-level toggle. When false (default),
	// 1067-byte XSym files written by macOS/Windows clients are stored as
	// regular files. When true, they are converted to real symlinks on CLOSE.
	// The conversion target is client-controlled, so promotion is opt-in.
	AllowMFsymlink bool
}
