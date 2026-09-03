package postgres

import (
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// dialect supplies postgres statement text and postgres error classification to
// the shared SQL core. It is stateless, so one package-level value serves every
// store and transaction.
type dialect struct{}

var pgDialect dialect

// IsNoRows reports pgx's empty-result sentinel.
func (dialect) IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Chunks returns the postgres file-chunk statements.
func (dialect) Chunks() *storesql.ChunkQueries { return &chunkQueries }

var chunkQueries = storesql.ChunkQueries{
	SelectByID:   selectFileChunkByID,
	SelectByHash: selectFileChunkByHash,
	Upsert:       putFileChunkQuery,
	Delete:       `DELETE FROM file_blocks WHERE id = $1`,
	IncrementRef: `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = $1`,
	DecrementRef: `UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`,
	// state = 2 (Remote) scoping mirrors SelectByHash and the memory/badger
	// backends: a Pending row is not a valid dedup donor.
	AddRef:             `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = $1 AND state = 2 /* Remote */`,
	ReapZeroRef:        `DELETE FROM file_blocks WHERE id = $1 AND ref_count = 0`,
	ListByPayloadRange: listFileChunksQuery,
	EnumerateHashes:    enumerateHashesQuery,
}

// MapError translates a driver error into an ExportError. It is this
// package's own mapPgError, reached through the Dialect so a shared body can
// classify an error without importing the driver that produced it.
func (dialect) MapError(err error, operation, path string) error {
	return mapPgError(err, operation, path)
}

// Files returns the postgres file and directory read statements.
func (dialect) Files() *storesql.FileQueries { return &fileQueries }

// inodeSelectColumns is the full inode projection GetFile and
// GetFileByPayloadID share, block-ref aggregate included, in the column order
// sqlcodec.FileRowToFileWithNlinkAndBlocks scans.
const inodeSelectColumns = `
	f.id, f.share_name, ` + inodePathExpr + `,
	f.file_type, f.mode, f.uid, f.gid, f.size,
	f.atime, f.mtime, f.ctime, f.creation_time,
	f.content_id, f.link_target, f.device_major, f.device_minor,
	f.hidden, f.acl, f.eas, f.object_id,
	f.deleted_at, f.original_path, f.deleted_by, f.nlink,
	` + blockRefsAggExpr + `
`

var fileQueries = storesql.FileQueries{
	GetFile: `SELECT ` + inodeSelectColumns + ` FROM inodes f
		WHERE f.id = $1 AND f.share_name = $2`,

	GetChild: `SELECT dc.child_id FROM parent_child_map dc
		WHERE dc.parent_id = $1 AND dc.child_name = $2`,

	GetParent: `SELECT parent_id FROM parent_child_map WHERE child_id = $1 LIMIT 1`,

	GetLinkCount: `SELECT nlink FROM inodes WHERE id = $1`,

	// f.acl and f.eas are hydrated here so DirEntry.Attr carries them, matching
	// the Memory and Badger backends: without those columns an ACL-aware caller
	// iterating a listing would silently fall back to POSIX mode bits. They are
	// small next to the row itself and save a GetFile per entry.
	ListChildren: `SELECT dc.child_name, dc.child_id, f.file_type, f.mode, f.uid, f.gid, f.size,
		       f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, f.acl, f.eas, f.object_id,
		       f.deleted_at, f.original_path, f.deleted_by, f.nlink
		FROM parent_child_map dc
		LEFT JOIN inodes f ON dc.child_id = f.id
		WHERE dc.parent_id = $1 AND dc.child_name > $2
		ORDER BY dc.child_name
		LIMIT $3`,
	ListChildNames: `SELECT dc.child_name, dc.child_id
		FROM parent_child_map dc
		WHERE dc.parent_id = $1 AND dc.child_name > $2
		ORDER BY dc.child_name
		LIMIT $3`,

	// The lookup goes through content_id_hash (an md5 of content_id) because a
	// content id for a path near PATH_MAX overruns postgres' 2704-byte btree
	// key limit, which would make the index unusable for the long paths that
	// need it most.
	SetChild: `INSERT INTO parent_child_map (parent_id, child_name, child_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (parent_id, child_name) DO UPDATE SET child_id = EXCLUDED.child_id`,

	DeleteChild: `DELETE FROM parent_child_map WHERE parent_id = $1 AND child_name = $2`,

	// inodes.nlink is the sole source of truth for the hard-link count, so
	// GETATTR reads it straight off the inode row without a join.
	SetLinkCount: `UPDATE inodes SET nlink = $1 WHERE id = $2`,
	FileUsageRow: `SELECT file_type, size, uid, gid, nlink FROM inodes WHERE id = $1 AND share_name = $2`,
	DeleteFile:   `DELETE FROM inodes WHERE id = $1 AND share_name = $2`,

	GetFileByPayloadID: `SELECT ` + inodeSelectColumns + ` FROM inodes f
		WHERE f.content_id_hash = md5($1)
		LIMIT 1`,
}

// Shares returns the postgres share read statements.
func (dialect) Shares() *storesql.ShareQueries { return &shareQueries }

func (dialect) Clients() *storesql.ClientQueries { return &clientQueries }

var clientQueries = storesql.ClientQueries{
	Put: `
		INSERT INTO nsm_client_registrations (
			client_id, mon_name, priv, callback_host, callback_prog,
			callback_vers, callback_proc, registered_at, server_epoch
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (client_id) DO UPDATE SET
			mon_name = EXCLUDED.mon_name,
			priv = EXCLUDED.priv,
			callback_host = EXCLUDED.callback_host,
			callback_prog = EXCLUDED.callback_prog,
			callback_vers = EXCLUDED.callback_vers,
			callback_proc = EXCLUDED.callback_proc,
			registered_at = EXCLUDED.registered_at,
			server_epoch = EXCLUDED.server_epoch`,
	Get: `
		SELECT client_id, mon_name, priv, callback_host, callback_prog,
		       callback_vers, callback_proc, registered_at, server_epoch
		FROM nsm_client_registrations
		WHERE client_id = $1`,
	Delete: `DELETE FROM nsm_client_registrations WHERE client_id = $1`,
	List: `
		SELECT client_id, mon_name, priv, callback_host, callback_prog,
		       callback_vers, callback_proc, registered_at, server_epoch
		FROM nsm_client_registrations
		ORDER BY registered_at`,
	DeleteAll:       `DELETE FROM nsm_client_registrations`,
	DeleteByMonName: `DELETE FROM nsm_client_registrations WHERE mon_name = $1`,
}

func (dialect) Recovery() *storesql.RecoveryQueries { return &recoveryQueries }

func (dialect) Durable() *storesql.DurableQueries { return &durableQueries }

var durableQueries = storesql.DurableQueries{
	Put: `
		INSERT INTO durable_handles (` + storesql.DurableHandleInsertColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)
		ON CONFLICT (id) DO UPDATE SET
			file_id = EXCLUDED.file_id,
			path = EXCLUDED.path,
			share_name = EXCLUDED.share_name,
			desired_access = EXCLUDED.desired_access,
			granted_access = EXCLUDED.granted_access,
			share_access = EXCLUDED.share_access,
			create_options = EXCLUDED.create_options,
			metadata_handle = EXCLUDED.metadata_handle,
			payload_id = EXCLUDED.payload_id,
			oplock_level = EXCLUDED.oplock_level,
			lease_key = EXCLUDED.lease_key,
			lease_state = EXCLUDED.lease_state,
			create_guid = EXCLUDED.create_guid,
			app_instance_id = EXCLUDED.app_instance_id,
			username = EXCLUDED.username,
			session_key_hash = EXCLUDED.session_key_hash,
			is_v2 = EXCLUDED.is_v2,
			created_at = EXCLUDED.created_at,
			disconnected_at = EXCLUDED.disconnected_at,
			timeout_ms = EXCLUDED.timeout_ms,
			server_start_time = EXCLUDED.server_start_time,
			delete_pending = EXCLUDED.delete_pending,
			parent_handle = EXCLUDED.parent_handle,
			file_name = EXCLUDED.file_name,
			is_directory = EXCLUDED.is_directory,
			position_info = EXCLUDED.position_info,
			original_file_id = EXCLUDED.original_file_id,
			requested_alloc_size = EXCLUDED.requested_alloc_size,
			lease_epoch = EXCLUDED.lease_epoch,
			is_persistent = EXCLUDED.is_persistent,
			client_guid = EXCLUDED.client_guid`,
	Get:             `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles WHERE id = $1`,
	GetByFileID:     `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles WHERE file_id = $1 ORDER BY id LIMIT 1`,
	GetByCreateGuid: `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles WHERE create_guid = $1 ORDER BY id LIMIT 1`,
	Consume:         `DELETE FROM durable_handles WHERE id = $1 RETURNING ` + storesql.DurableHandleColumns,

	ListByAppInstanceId: `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles WHERE app_instance_id = $1 ORDER BY created_at`,
	ListByFileHandle:    `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles WHERE metadata_handle = $1 ORDER BY created_at`,
	List:                `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles ORDER BY created_at`,
	ListByShare:         `SELECT ` + storesql.DurableHandleColumns + ` FROM durable_handles WHERE share_name = $1 ORDER BY created_at`,

	Delete:     `DELETE FROM durable_handles WHERE id = $1`,
	DeleteByID: `DELETE FROM durable_handles WHERE id = $1`,

	ExpiryCandidates: `SELECT id, disconnected_at, timeout_ms FROM durable_handles`,
	DeleteExpired:    `DELETE FROM durable_handles WHERE disconnected_at + (timeout_ms || ' milliseconds')::interval <= $1`,
}

var recoveryQueries = storesql.RecoveryQueries{
	Put: `
		INSERT INTO v4_client_recovery (
			clientid_string, clientid, boot_verifier, principal,
			confirmed_at, server_epoch, reclaim_complete
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (clientid_string) DO UPDATE SET
			clientid = EXCLUDED.clientid,
			boot_verifier = EXCLUDED.boot_verifier,
			principal = EXCLUDED.principal,
			confirmed_at = EXCLUDED.confirmed_at,
			server_epoch = EXCLUDED.server_epoch,
			reclaim_complete = EXCLUDED.reclaim_complete`,
	Delete: `DELETE FROM v4_client_recovery WHERE clientid_string = $1`,
	List: `
		SELECT clientid_string, clientid, boot_verifier, principal,
		       confirmed_at, server_epoch, reclaim_complete
		FROM v4_client_recovery
		ORDER BY confirmed_at`,
	RecordReclaimComplete: `UPDATE v4_client_recovery SET reclaim_complete = TRUE WHERE clientid_string = $1`,
}

func (dialect) Server() *storesql.ServerQueries { return &serverQueries }

var serverQueries = storesql.ServerQueries{
	GetServerConfig: `SELECT config FROM server_config WHERE id = 1`,
	SetServerConfig: `INSERT INTO server_config (id, config)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET config = EXCLUDED.config, updated_at = NOW()`,
}

var shareQueries = storesql.ShareQueries{
	GetRootHandle:     `SELECT root_file_id FROM shares WHERE share_name = $1`,
	GetShareOptions:   `SELECT options FROM shares WHERE share_name = $1`,
	ListShares:        `SELECT share_name FROM shares`,
	GetFilesystemMeta: `SELECT meta FROM filesystem_meta WHERE share_name = $1`,
	Statfs:            `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE share_name = $1 AND file_type = $2`,
	PutFilesystemMeta: `INSERT INTO filesystem_meta (share_name, meta)
		VALUES ($1, $2)
		ON CONFLICT (share_name) DO UPDATE SET meta = EXCLUDED.meta`,
	SetShareOptions:   `UPDATE shares SET options = $1 WHERE share_name = $2`,
	DeleteShare:       `DELETE FROM shares WHERE share_name = $1`,
	DeleteShareInodes: `DELETE FROM inodes WHERE share_name = $1`,
	ShareQuotaFreed: `SELECT %s, COALESCE(SUM(size), 0), COUNT(*) FROM inodes
		WHERE share_name = $1 AND file_type = $2 AND nlink > 0 GROUP BY %s`,
}

var _ storesql.Dialect = dialect{}

func (dialect) Locks() *storesql.LockQueries { return &lockQueries }

var lockQueries = storesql.LockQueries{
	Put: `
		INSERT INTO locks (` + storesql.LockColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
		        $14, $15, $16, $17, $18, $19, $20, $21, $22,
		        $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (id) DO UPDATE SET
			share_name = EXCLUDED.share_name,
			file_id = EXCLUDED.file_id,
			owner_id = EXCLUDED.owner_id,
			client_id = EXCLUDED.client_id,
			lock_type = EXCLUDED.lock_type,
			byte_offset = EXCLUDED.byte_offset,
			byte_length = EXCLUDED.byte_length,
			is_zero_byte = EXCLUDED.is_zero_byte,
			is_legacy_byte_range = EXCLUDED.is_legacy_byte_range,
			share_reservation = EXCLUDED.share_reservation,
			acquired_at = EXCLUDED.acquired_at,
			server_epoch = EXCLUDED.server_epoch,
			lease_key = EXCLUDED.lease_key,
			lease_state = EXCLUDED.lease_state,
			lease_epoch = EXCLUDED.lease_epoch,
			break_to_state = EXCLUDED.break_to_state,
			breaking_to_required = EXCLUDED.breaking_to_required,
			breaking = EXCLUDED.breaking,
			parent_lease_key = EXCLUDED.parent_lease_key,
			is_directory = EXCLUDED.is_directory,
			is_traditional_oplock = EXCLUDED.is_traditional_oplock,
			delegation_id = EXCLUDED.delegation_id,
			deleg_type = EXCLUDED.deleg_type,
			deleg_breaking = EXCLUDED.deleg_breaking,
			deleg_recalled = EXCLUDED.deleg_recalled,
			deleg_revoked = EXCLUDED.deleg_revoked,
			deleg_notification_mask = EXCLUDED.deleg_notification_mask,
			break_started = EXCLUDED.break_started`,
	SelectByID:     `SELECT ` + storesql.LockColumns + ` FROM locks WHERE id = $1`,
	Delete:         `DELETE FROM locks WHERE id = $1`,
	DeleteByClient: `DELETE FROM locks WHERE client_id = $1`,
	DeleteByFile:   `DELETE FROM locks WHERE file_id = $1`,
	IncrementEpoch: `
		INSERT INTO server_epoch (id, epoch, updated_at)
		VALUES (1, 1, NOW())
		ON CONFLICT (id) DO UPDATE SET
			epoch = server_epoch.epoch + 1,
			updated_at = NOW()
		RETURNING epoch`,
	SetCleanShutdown: `
		INSERT INTO server_epoch (id, epoch, clean_shutdown, updated_at)
		VALUES (1, 0, $1, NOW())
		ON CONFLICT (id) DO UPDATE SET
			clean_shutdown = EXCLUDED.clean_shutdown,
			updated_at = NOW()`,
	ListWhere: lockListWhere,
}

// lockListWhere renders a LockQuery as numbered `$N` placeholders, counting up
// in the order the arguments are appended.
func lockListWhere(query lock.LockQuery) (string, []any) {
	var where string
	var args []any

	add := func(column string, value any) {
		args = append(args, value)
		where += ` AND ` + column + ` = $` + strconv.Itoa(len(args))
	}

	if query.FileID != "" {
		add("file_id", query.FileID)
	}
	if query.OwnerID != "" {
		add("owner_id", query.OwnerID)
	}
	if query.ClientID != "" {
		add("client_id", query.ClientID)
	}
	if query.ShareName != "" {
		add("share_name", query.ShareName)
	}

	return where, args
}
