package types

import (
	goerrors "errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ============================================================================
// Error Code Value Tests
// ============================================================================

func TestErrorCodeValues(t *testing.T) {
	tests := []struct {
		name     string
		code     uint32
		expected uint32
	}{
		{"NFS4_OK", NFS4_OK, 0},
		{"NFS4ERR_PERM", NFS4ERR_PERM, 1},
		{"NFS4ERR_NOENT", NFS4ERR_NOENT, 2},
		{"NFS4ERR_IO", NFS4ERR_IO, 5},
		{"NFS4ERR_ACCESS", NFS4ERR_ACCESS, 13},
		{"NFS4ERR_EXIST", NFS4ERR_EXIST, 17},
		{"NFS4ERR_NOTDIR", NFS4ERR_NOTDIR, 20},
		{"NFS4ERR_ISDIR", NFS4ERR_ISDIR, 21},
		{"NFS4ERR_INVAL", NFS4ERR_INVAL, 22},
		{"NFS4ERR_NOSPC", NFS4ERR_NOSPC, 28},
		{"NFS4ERR_ROFS", NFS4ERR_ROFS, 30},
		{"NFS4ERR_NAMETOOLONG", NFS4ERR_NAMETOOLONG, 63},
		{"NFS4ERR_NOTEMPTY", NFS4ERR_NOTEMPTY, 66},
		{"NFS4ERR_DQUOT", NFS4ERR_DQUOT, 69},
		{"NFS4ERR_STALE", NFS4ERR_STALE, 70},
		{"NFS4ERR_BADHANDLE", NFS4ERR_BADHANDLE, 10001},
		{"NFS4ERR_NOTSUPP", NFS4ERR_NOTSUPP, 10004},
		{"NFS4ERR_SERVERFAULT", NFS4ERR_SERVERFAULT, 10006},
		{"NFS4ERR_LOCKED", NFS4ERR_LOCKED, 10012},
		{"NFS4ERR_GRACE", NFS4ERR_GRACE, 10013},
		{"NFS4ERR_NOFILEHANDLE", NFS4ERR_NOFILEHANDLE, 10020},
		{"NFS4ERR_MINOR_VERS_MISMATCH", NFS4ERR_MINOR_VERS_MISMATCH, 10021},
		{"NFS4ERR_BADCHAR", NFS4ERR_BADCHAR, 10040},
		{"NFS4ERR_BADNAME", NFS4ERR_BADNAME, 10041},
		{"NFS4ERR_OP_ILLEGAL", NFS4ERR_OP_ILLEGAL, 10044},
		{"NFS4ERR_DEADLOCK", NFS4ERR_DEADLOCK, 10045},
		{"NFS4ERR_CB_PATH_DOWN", NFS4ERR_CB_PATH_DOWN, 10048},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.code != tt.expected {
				t.Errorf("%s = %d, want %d", tt.name, tt.code, tt.expected)
			}
		})
	}
}

// ============================================================================
// Operation Number Tests
// ============================================================================

func TestOperationNumbers(t *testing.T) {
	tests := []struct {
		name     string
		op       uint32
		expected uint32
	}{
		{"OP_ACCESS", OP_ACCESS, 3},
		{"OP_CLOSE", OP_CLOSE, 4},
		{"OP_GETATTR", OP_GETATTR, 9},
		{"OP_GETFH", OP_GETFH, 10},
		{"OP_LOOKUP", OP_LOOKUP, 15},
		{"OP_PUTFH", OP_PUTFH, 22},
		{"OP_PUTROOTFH", OP_PUTROOTFH, 24},
		{"OP_READ", OP_READ, 25},
		{"OP_READDIR", OP_READDIR, 26},
		{"OP_RESTOREFH", OP_RESTOREFH, 31},
		{"OP_SAVEFH", OP_SAVEFH, 32},
		{"OP_SETCLIENTID", OP_SETCLIENTID, 35},
		{"OP_WRITE", OP_WRITE, 38},
		{"OP_RELEASE_LOCKOWNER", OP_RELEASE_LOCKOWNER, 39},
		{"OP_ILLEGAL", OP_ILLEGAL, 10044},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.op != tt.expected {
				t.Errorf("%s = %d, want %d", tt.name, tt.op, tt.expected)
			}
		})
	}
}

// ============================================================================
// RequireCurrentFH Tests
// ============================================================================

func TestRequireCurrentFH(t *testing.T) {
	t.Run("nil filehandle returns NFS4ERR_NOFILEHANDLE", func(t *testing.T) {
		ctx := &CompoundContext{CurrentFH: nil}
		status := RequireCurrentFH(ctx)
		if status != NFS4ERR_NOFILEHANDLE {
			t.Errorf("RequireCurrentFH(nil) = %d, want %d", status, NFS4ERR_NOFILEHANDLE)
		}
	})

	t.Run("non-nil filehandle returns NFS4_OK", func(t *testing.T) {
		ctx := &CompoundContext{CurrentFH: []byte("some-handle")}
		status := RequireCurrentFH(ctx)
		if status != NFS4_OK {
			t.Errorf("RequireCurrentFH(non-nil) = %d, want %d (NFS4_OK)", status, NFS4_OK)
		}
	})

	t.Run("empty filehandle returns NFS4_OK", func(t *testing.T) {
		// An empty but non-nil handle is technically "set"
		ctx := &CompoundContext{CurrentFH: []byte{}}
		status := RequireCurrentFH(ctx)
		if status != NFS4_OK {
			t.Errorf("RequireCurrentFH(empty) = %d, want %d (NFS4_OK)", status, NFS4_OK)
		}
	})
}

// ============================================================================
// RequireSavedFH Tests
// ============================================================================

func TestRequireSavedFH(t *testing.T) {
	t.Run("nil saved FH returns NFS4ERR_RESTOREFH", func(t *testing.T) {
		ctx := &CompoundContext{SavedFH: nil}
		status := RequireSavedFH(ctx)
		if status != NFS4ERR_RESTOREFH {
			t.Errorf("RequireSavedFH(nil) = %d, want %d", status, NFS4ERR_RESTOREFH)
		}
	})

	t.Run("non-nil saved FH returns NFS4_OK", func(t *testing.T) {
		ctx := &CompoundContext{SavedFH: []byte("saved-handle")}
		status := RequireSavedFH(ctx)
		if status != NFS4_OK {
			t.Errorf("RequireSavedFH(non-nil) = %d, want %d (NFS4_OK)", status, NFS4_OK)
		}
	})
}

// ============================================================================
// MapMetadataErrorToNFS4 Tests
// ============================================================================

func TestMapMetadataErrorToNFS4(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected uint32
	}{
		{
			name:     "nil error returns NFS4_OK",
			err:      nil,
			expected: NFS4_OK,
		},
		{
			name:     "ErrNotFound maps to NFS4ERR_NOENT",
			err:      errors.NewNotFoundError("/test", "file"),
			expected: NFS4ERR_NOENT,
		},
		{
			name:     "ErrAccessDenied maps to NFS4ERR_ACCESS",
			err:      errors.NewAccessDeniedError("denied"),
			expected: NFS4ERR_ACCESS,
		},
		{
			name:     "ErrPermissionDenied maps to NFS4ERR_PERM",
			err:      errors.NewPermissionDeniedError("/test"),
			expected: NFS4ERR_PERM,
		},
		{
			name:     "ErrAlreadyExists maps to NFS4ERR_EXIST",
			err:      errors.NewAlreadyExistsError("/test"),
			expected: NFS4ERR_EXIST,
		},
		{
			name:     "ErrNotEmpty maps to NFS4ERR_NOTEMPTY",
			err:      errors.NewNotEmptyError("/test"),
			expected: NFS4ERR_NOTEMPTY,
		},
		{
			name:     "ErrIsDirectory maps to NFS4ERR_ISDIR",
			err:      errors.NewIsDirectoryError("/test"),
			expected: NFS4ERR_ISDIR,
		},
		{
			name:     "ErrNotDirectory maps to NFS4ERR_NOTDIR",
			err:      errors.NewNotDirectoryError("/test"),
			expected: NFS4ERR_NOTDIR,
		},
		{
			name:     "ErrInvalidArgument maps to NFS4ERR_INVAL",
			err:      errors.NewInvalidArgumentError("bad arg"),
			expected: NFS4ERR_INVAL,
		},
		{
			name:     "ErrStaleHandle maps to NFS4ERR_STALE",
			err:      &errors.StoreError{Code: errors.ErrStaleHandle, Message: "stale"},
			expected: NFS4ERR_STALE,
		},
		{
			name:     "ErrInvalidHandle maps to NFS4ERR_BADHANDLE",
			err:      errors.NewInvalidHandleError(),
			expected: NFS4ERR_BADHANDLE,
		},
		{
			name:     "ErrNameTooLong maps to NFS4ERR_NAMETOOLONG",
			err:      errors.NewNameTooLongError("/test"),
			expected: NFS4ERR_NAMETOOLONG,
		},
		{
			name:     "ErrNoSpace maps to NFS4ERR_NOSPC",
			err:      &errors.StoreError{Code: errors.ErrNoSpace, Message: "no space"},
			expected: NFS4ERR_NOSPC,
		},
		{
			name:     "ErrQuotaExceeded maps to NFS4ERR_DQUOT",
			err:      errors.NewQuotaExceededError("/test"),
			expected: NFS4ERR_DQUOT,
		},
		{
			name:     "ErrReadOnly maps to NFS4ERR_ROFS",
			err:      &errors.StoreError{Code: errors.ErrReadOnly, Message: "ro"},
			expected: NFS4ERR_ROFS,
		},
		{
			name:     "ErrNotSupported maps to NFS4ERR_NOTSUPP",
			err:      &errors.StoreError{Code: errors.ErrNotSupported, Message: "unsupported"},
			expected: NFS4ERR_NOTSUPP,
		},
		{
			name:     "ErrLocked maps to NFS4ERR_LOCKED",
			err:      &errors.StoreError{Code: errors.ErrLocked, Message: "locked"},
			expected: NFS4ERR_LOCKED,
		},
		{
			name:     "ErrDeadlock maps to NFS4ERR_DEADLOCK",
			err:      &errors.StoreError{Code: errors.ErrDeadlock, Message: "deadlock"},
			expected: NFS4ERR_DEADLOCK,
		},
		{
			name:     "ErrGracePeriod maps to NFS4ERR_GRACE",
			err:      &errors.StoreError{Code: errors.ErrGracePeriod, Message: "grace"},
			expected: NFS4ERR_GRACE,
		},
		{
			name:     "ErrIOError maps to NFS4ERR_IO",
			err:      &errors.StoreError{Code: errors.ErrIOError, Message: "io error"},
			expected: NFS4ERR_IO,
		},
		{
			name:     "unknown StoreError code maps to NFS4ERR_SERVERFAULT",
			err:      &errors.StoreError{Code: errors.ErrConnectionLimitReached, Message: "limit"},
			expected: NFS4ERR_SERVERFAULT,
		},
		{
			name:     "non-StoreError maps to NFS4ERR_SERVERFAULT",
			err:      goerrors.New("random error"),
			expected: NFS4ERR_SERVERFAULT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MapMetadataErrorToNFS4(tt.err)
			if result != tt.expected {
				t.Errorf("MapMetadataErrorToNFS4(%v) = %d, want %d", tt.err, result, tt.expected)
			}
		})
	}
}

// ============================================================================
// ValidateUTF8Filename Tests
// ============================================================================

func TestValidateUTF8Filename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		expected uint32
	}{
		{
			name:     "valid ASCII filename",
			filename: "hello.txt",
			expected: NFS4_OK,
		},
		{
			name:     "valid UTF-8 with unicode",
			filename: "file-\u00e9\u00e8\u00ea.txt",
			expected: NFS4_OK,
		},
		{
			name:     "valid UTF-8 with CJK characters",
			filename: "\u6587\u4ef6.txt",
			expected: NFS4_OK,
		},
		{
			name:     "valid 255-byte filename",
			filename: strings.Repeat("a", 255),
			expected: NFS4_OK,
		},
		{
			name:     "empty filename",
			filename: "",
			expected: NFS4ERR_INVAL,
		},
		{
			name:     "invalid UTF-8 bytes",
			filename: string([]byte{0xff, 0xfe, 0xfd}),
			expected: NFS4ERR_BADCHAR,
		},
		{
			name:     "contains null byte",
			filename: "hello\x00world",
			expected: NFS4ERR_BADCHAR,
		},
		{
			name:     "contains forward slash",
			filename: "hello/world",
			expected: NFS4ERR_BADNAME,
		},
		{
			name:     "just a slash",
			filename: "/",
			expected: NFS4ERR_BADNAME,
		},
		{
			name:     "too long filename (256 bytes)",
			filename: strings.Repeat("x", 256),
			expected: NFS4ERR_NAMETOOLONG,
		},
		{
			name:     "very long filename (1000 bytes)",
			filename: strings.Repeat("y", 1000),
			expected: NFS4ERR_NAMETOOLONG,
		},
		{
			name:     "single character filename",
			filename: "a",
			expected: NFS4_OK,
		},
		{
			name:     "filename with dots",
			filename: "..hidden",
			expected: NFS4_OK,
		},
		{
			name:     "filename with spaces",
			filename: "my file.txt",
			expected: NFS4_OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateUTF8Filename(tt.filename)
			if result != tt.expected {
				t.Errorf("ValidateUTF8Filename(%q) = %d, want %d", tt.filename, result, tt.expected)
			}
		})
	}
}

// ============================================================================
// OpName Tests
// ============================================================================

func TestOpName(t *testing.T) {
	tests := []struct {
		op       uint32
		expected string
	}{
		{OP_ACCESS, "ACCESS"},
		{OP_GETATTR, "GETATTR"},
		{OP_LOOKUP, "LOOKUP"},
		{OP_PUTFH, "PUTFH"},
		{OP_PUTROOTFH, "PUTROOTFH"},
		{OP_ILLEGAL, "ILLEGAL"},
		{9999, "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			name := OpName(tt.op)
			if name != tt.expected {
				t.Errorf("OpName(%d) = %q, want %q", tt.op, name, tt.expected)
			}
		})
	}
}

// ============================================================================
// NFSv4.1 Operation Number Tests
// ============================================================================

func TestV41OperationNumbers(t *testing.T) {
	tests := []struct {
		name     string
		op       uint32
		expected uint32
	}{
		{"OP_BACKCHANNEL_CTL", OP_BACKCHANNEL_CTL, 40},
		{"OP_BIND_CONN_TO_SESSION", OP_BIND_CONN_TO_SESSION, 41},
		{"OP_EXCHANGE_ID", OP_EXCHANGE_ID, 42},
		{"OP_CREATE_SESSION", OP_CREATE_SESSION, 43},
		{"OP_DESTROY_SESSION", OP_DESTROY_SESSION, 44},
		{"OP_FREE_STATEID", OP_FREE_STATEID, 45},
		{"OP_GET_DIR_DELEGATION", OP_GET_DIR_DELEGATION, 46},
		{"OP_GETDEVICEINFO", OP_GETDEVICEINFO, 47},
		{"OP_GETDEVICELIST", OP_GETDEVICELIST, 48},
		{"OP_LAYOUTCOMMIT", OP_LAYOUTCOMMIT, 49},
		{"OP_LAYOUTGET", OP_LAYOUTGET, 50},
		{"OP_LAYOUTRETURN", OP_LAYOUTRETURN, 51},
		{"OP_SECINFO_NO_NAME", OP_SECINFO_NO_NAME, 52},
		{"OP_SEQUENCE", OP_SEQUENCE, 53},
		{"OP_SET_SSV", OP_SET_SSV, 54},
		{"OP_TEST_STATEID", OP_TEST_STATEID, 55},
		{"OP_WANT_DELEGATION", OP_WANT_DELEGATION, 56},
		{"OP_DESTROY_CLIENTID", OP_DESTROY_CLIENTID, 57},
		{"OP_RECLAIM_COMPLETE", OP_RECLAIM_COMPLETE, 58},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.op != tt.expected {
				t.Errorf("%s = %d, want %d", tt.name, tt.op, tt.expected)
			}
		})
	}
}

// ============================================================================
// NFSv4.1 OpName Tests
// ============================================================================

func TestV41OpName(t *testing.T) {
	// All 19 v4.1 operations must have human-readable names (not "UNKNOWN")
	v41Ops := []struct {
		op       uint32
		expected string
	}{
		{OP_BACKCHANNEL_CTL, "BACKCHANNEL_CTL"},
		{OP_BIND_CONN_TO_SESSION, "BIND_CONN_TO_SESSION"},
		{OP_EXCHANGE_ID, "EXCHANGE_ID"},
		{OP_CREATE_SESSION, "CREATE_SESSION"},
		{OP_DESTROY_SESSION, "DESTROY_SESSION"},
		{OP_FREE_STATEID, "FREE_STATEID"},
		{OP_GET_DIR_DELEGATION, "GET_DIR_DELEGATION"},
		{OP_GETDEVICEINFO, "GETDEVICEINFO"},
		{OP_GETDEVICELIST, "GETDEVICELIST"},
		{OP_LAYOUTCOMMIT, "LAYOUTCOMMIT"},
		{OP_LAYOUTGET, "LAYOUTGET"},
		{OP_LAYOUTRETURN, "LAYOUTRETURN"},
		{OP_SECINFO_NO_NAME, "SECINFO_NO_NAME"},
		{OP_SEQUENCE, "SEQUENCE"},
		{OP_SET_SSV, "SET_SSV"},
		{OP_TEST_STATEID, "TEST_STATEID"},
		{OP_WANT_DELEGATION, "WANT_DELEGATION"},
		{OP_DESTROY_CLIENTID, "DESTROY_CLIENTID"},
		{OP_RECLAIM_COMPLETE, "RECLAIM_COMPLETE"},
	}

	for _, tt := range v41Ops {
		t.Run(tt.expected, func(t *testing.T) {
			name := OpName(tt.op)
			if name != tt.expected {
				t.Errorf("OpName(%d) = %q, want %q", tt.op, name, tt.expected)
			}
			if name == "UNKNOWN" {
				t.Errorf("OpName(%d) returned UNKNOWN for v4.1 operation %s", tt.op, tt.expected)
			}
		})
	}
}

// ============================================================================
// NFSv4.1 OpNameToNum Tests
// ============================================================================

func TestV41OpNameToNum(t *testing.T) {
	// All 19 v4.1 operations must be in opNameToNum
	v41Names := map[string]uint32{
		"BACKCHANNEL_CTL":      OP_BACKCHANNEL_CTL,
		"BIND_CONN_TO_SESSION": OP_BIND_CONN_TO_SESSION,
		"EXCHANGE_ID":          OP_EXCHANGE_ID,
		"CREATE_SESSION":       OP_CREATE_SESSION,
		"DESTROY_SESSION":      OP_DESTROY_SESSION,
		"FREE_STATEID":         OP_FREE_STATEID,
		"GET_DIR_DELEGATION":   OP_GET_DIR_DELEGATION,
		"GETDEVICEINFO":        OP_GETDEVICEINFO,
		"GETDEVICELIST":        OP_GETDEVICELIST,
		"LAYOUTCOMMIT":         OP_LAYOUTCOMMIT,
		"LAYOUTGET":            OP_LAYOUTGET,
		"LAYOUTRETURN":         OP_LAYOUTRETURN,
		"SECINFO_NO_NAME":      OP_SECINFO_NO_NAME,
		"SEQUENCE":             OP_SEQUENCE,
		"SET_SSV":              OP_SET_SSV,
		"TEST_STATEID":         OP_TEST_STATEID,
		"WANT_DELEGATION":      OP_WANT_DELEGATION,
		"DESTROY_CLIENTID":     OP_DESTROY_CLIENTID,
		"RECLAIM_COMPLETE":     OP_RECLAIM_COMPLETE,
	}

	for name, expectedOp := range v41Names {
		t.Run(name, func(t *testing.T) {
			op, ok := OpNameToNum(name)
			if !ok {
				t.Errorf("OpNameToNum(%q) not found", name)
			}
			if op != expectedOp {
				t.Errorf("OpNameToNum(%q) = %d, want %d", name, op, expectedOp)
			}
		})
	}
}

// ============================================================================
// NFSv4.0 OpName/OpNameToNum Regression Tests
// ============================================================================

func TestV40OpNameRegression(t *testing.T) {
	// Verify existing v4.0 operations still work after v4.1 additions
	v40Ops := map[uint32]string{
		OP_ACCESS:              "ACCESS",
		OP_CLOSE:               "CLOSE",
		OP_COMMIT:              "COMMIT",
		OP_CREATE:              "CREATE",
		OP_DELEGPURGE:          "DELEGPURGE",
		OP_DELEGRETURN:         "DELEGRETURN",
		OP_GETATTR:             "GETATTR",
		OP_GETFH:               "GETFH",
		OP_LINK:                "LINK",
		OP_LOCK:                "LOCK",
		OP_LOCKT:               "LOCKT",
		OP_LOCKU:               "LOCKU",
		OP_LOOKUP:              "LOOKUP",
		OP_LOOKUPP:             "LOOKUPP",
		OP_NVERIFY:             "NVERIFY",
		OP_OPEN:                "OPEN",
		OP_OPENATTR:            "OPENATTR",
		OP_OPEN_CONFIRM:        "OPEN_CONFIRM",
		OP_OPEN_DOWNGRADE:      "OPEN_DOWNGRADE",
		OP_PUTFH:               "PUTFH",
		OP_PUTPUBFH:            "PUTPUBFH",
		OP_PUTROOTFH:           "PUTROOTFH",
		OP_READ:                "READ",
		OP_READDIR:             "READDIR",
		OP_READLINK:            "READLINK",
		OP_REMOVE:              "REMOVE",
		OP_RENAME:              "RENAME",
		OP_RENEW:               "RENEW",
		OP_RESTOREFH:           "RESTOREFH",
		OP_SAVEFH:              "SAVEFH",
		OP_SECINFO:             "SECINFO",
		OP_SETATTR:             "SETATTR",
		OP_SETCLIENTID:         "SETCLIENTID",
		OP_SETCLIENTID_CONFIRM: "SETCLIENTID_CONFIRM",
		OP_VERIFY:              "VERIFY",
		OP_WRITE:               "WRITE",
		OP_RELEASE_LOCKOWNER:   "RELEASE_LOCKOWNER",
	}

	for op, expectedName := range v40Ops {
		t.Run(expectedName, func(t *testing.T) {
			name := OpName(op)
			if name != expectedName {
				t.Errorf("OpName(%d) = %q, want %q (v4.0 regression)", op, name, expectedName)
			}
			num, ok := OpNameToNum(expectedName)
			if !ok {
				t.Errorf("OpNameToNum(%q) not found (v4.0 regression)", expectedName)
			}
			if num != op {
				t.Errorf("OpNameToNum(%q) = %d, want %d (v4.0 regression)", expectedName, num, op)
			}
		})
	}
}

// ============================================================================
// NFSv4.1 Error Code Value Tests
// ============================================================================

func TestV41ErrorCodeValues(t *testing.T) {
	tests := []struct {
		name     string
		code     uint32
		expected uint32
	}{
		{"NFS4ERR_BADIOMODE", NFS4ERR_BADIOMODE, 10049},
		{"NFS4ERR_BADLAYOUT", NFS4ERR_BADLAYOUT, 10050},
		{"NFS4ERR_BAD_SESSION_DIGEST", NFS4ERR_BAD_SESSION_DIGEST, 10051},
		{"NFS4ERR_BADSESSION", NFS4ERR_BADSESSION, 10052},
		{"NFS4ERR_BADSLOT", NFS4ERR_BADSLOT, 10053},
		{"NFS4ERR_COMPLETE_ALREADY", NFS4ERR_COMPLETE_ALREADY, 10054},
		{"NFS4ERR_CONN_NOT_BOUND_TO_SESSION", NFS4ERR_CONN_NOT_BOUND_TO_SESSION, 10055},
		{"NFS4ERR_DELEG_ALREADY_WANTED", NFS4ERR_DELEG_ALREADY_WANTED, 10056},
		{"NFS4ERR_BACK_CHAN_BUSY", NFS4ERR_BACK_CHAN_BUSY, 10057},
		{"NFS4ERR_LAYOUTTRYLATER", NFS4ERR_LAYOUTTRYLATER, 10058},
		{"NFS4ERR_LAYOUTUNAVAILABLE", NFS4ERR_LAYOUTUNAVAILABLE, 10059},
		{"NFS4ERR_NOMATCHING_LAYOUT", NFS4ERR_NOMATCHING_LAYOUT, 10060},
		{"NFS4ERR_RECALLCONFLICT", NFS4ERR_RECALLCONFLICT, 10061},
		{"NFS4ERR_UNKNOWN_LAYOUTTYPE", NFS4ERR_UNKNOWN_LAYOUTTYPE, 10062},
		{"NFS4ERR_SEQ_MISORDERED", NFS4ERR_SEQ_MISORDERED, 10063},
		{"NFS4ERR_SEQUENCE_POS", NFS4ERR_SEQUENCE_POS, 10064},
		{"NFS4ERR_REQ_TOO_BIG", NFS4ERR_REQ_TOO_BIG, 10065},
		{"NFS4ERR_REP_TOO_BIG", NFS4ERR_REP_TOO_BIG, 10066},
		{"NFS4ERR_REP_TOO_BIG_TO_CACHE", NFS4ERR_REP_TOO_BIG_TO_CACHE, 10067},
		{"NFS4ERR_RETRY_UNCACHED_REP", NFS4ERR_RETRY_UNCACHED_REP, 10068},
		{"NFS4ERR_UNSAFE_COMPOUND", NFS4ERR_UNSAFE_COMPOUND, 10069},
		{"NFS4ERR_TOO_MANY_OPS", NFS4ERR_TOO_MANY_OPS, 10070},
		{"NFS4ERR_OP_NOT_IN_SESSION", NFS4ERR_OP_NOT_IN_SESSION, 10071},
		{"NFS4ERR_HASH_ALG_UNSUPP", NFS4ERR_HASH_ALG_UNSUPP, 10072},
		{"NFS4ERR_CLIENTID_BUSY", NFS4ERR_CLIENTID_BUSY, 10074},
		{"NFS4ERR_PNFS_IO_HOLE", NFS4ERR_PNFS_IO_HOLE, 10075},
		{"NFS4ERR_SEQ_FALSE_RETRY", NFS4ERR_SEQ_FALSE_RETRY, 10076},
		{"NFS4ERR_BAD_HIGH_SLOT", NFS4ERR_BAD_HIGH_SLOT, 10077},
		{"NFS4ERR_DEADSESSION", NFS4ERR_DEADSESSION, 10078},
		{"NFS4ERR_ENCR_ALG_UNSUPP", NFS4ERR_ENCR_ALG_UNSUPP, 10079},
		{"NFS4ERR_PNFS_NO_LAYOUT", NFS4ERR_PNFS_NO_LAYOUT, 10080},
		{"NFS4ERR_NOT_ONLY_OP", NFS4ERR_NOT_ONLY_OP, 10081},
		{"NFS4ERR_WRONG_CRED", NFS4ERR_WRONG_CRED, 10082},
		{"NFS4ERR_WRONG_TYPE", NFS4ERR_WRONG_TYPE, 10083},
		{"NFS4ERR_DIRDELEG_UNAVAIL", NFS4ERR_DIRDELEG_UNAVAIL, 10084},
		{"NFS4ERR_REJECT_DELEG", NFS4ERR_REJECT_DELEG, 10085},
		{"NFS4ERR_RETURNCONFLICT", NFS4ERR_RETURNCONFLICT, 10086},
		{"NFS4ERR_DELEG_REVOKED", NFS4ERR_DELEG_REVOKED, 10087},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.code != tt.expected {
				t.Errorf("%s = %d, want %d", tt.name, tt.code, tt.expected)
			}
		})
	}
}

// ============================================================================
// NFSv4.1 CbOpName Tests
// ============================================================================

func TestCbOpName(t *testing.T) {
	tests := []struct {
		op       uint32
		expected string
	}{
		{OP_CB_GETATTR, "CB_GETATTR"},
		{OP_CB_RECALL, "CB_RECALL"},
		{CB_LAYOUTRECALL, "CB_LAYOUTRECALL"},
		{CB_NOTIFY, "CB_NOTIFY"},
		{CB_PUSH_DELEG, "CB_PUSH_DELEG"},
		{CB_RECALL_ANY, "CB_RECALL_ANY"},
		{CB_RECALLABLE_OBJ_AVAIL, "CB_RECALLABLE_OBJ_AVAIL"},
		{CB_RECALL_SLOT, "CB_RECALL_SLOT"},
		{CB_SEQUENCE, "CB_SEQUENCE"},
		{CB_WANTS_CANCELLED, "CB_WANTS_CANCELLED"},
		{CB_NOTIFY_LOCK, "CB_NOTIFY_LOCK"},
		{CB_NOTIFY_DEVICEID, "CB_NOTIFY_DEVICEID"},
		{9999, "CB_UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			name := CbOpName(tt.op)
			if name != tt.expected {
				t.Errorf("CbOpName(%d) = %q, want %q", tt.op, name, tt.expected)
			}
		})
	}
}

// ============================================================================
// NFSv4.1 Minor Version Constant Test
// ============================================================================

func TestV41MinorVersion(t *testing.T) {
	if NFS4_MINOR_VERSION_1 != 1 {
		t.Errorf("NFS4_MINOR_VERSION_1 = %d, want 1", NFS4_MINOR_VERSION_1)
	}
	if NFS4_SESSIONID_SIZE != 16 {
		t.Errorf("NFS4_SESSIONID_SIZE = %d, want 16", NFS4_SESSIONID_SIZE)
	}
}

// ============================================================================
// Protocol Limits Tests
// ============================================================================

func TestProtocolLimits(t *testing.T) {
	if NFS4_FHSIZE != 128 {
		t.Errorf("NFS4_FHSIZE = %d, want 128", NFS4_FHSIZE)
	}
	if MaxCompoundOps != 128 {
		t.Errorf("MaxCompoundOps = %d, want 128", MaxCompoundOps)
	}
	if NFS4_MINOR_VERSION_0 != 0 {
		t.Errorf("NFS4_MINOR_VERSION_0 = %d, want 0", NFS4_MINOR_VERSION_0)
	}
	if FH4_PERSISTENT != 0x00 {
		t.Errorf("FH4_PERSISTENT = 0x%02x, want 0x00", FH4_PERSISTENT)
	}
	if FH4_VOLATILE_ANY != 0x01 {
		t.Errorf("FH4_VOLATILE_ANY = 0x%02x, want 0x01", FH4_VOLATILE_ANY)
	}
}

// ============================================================================
// File Type Constants Tests
// ============================================================================

func TestFileTypeConstants(t *testing.T) {
	if NF4REG != 1 {
		t.Errorf("NF4REG = %d, want 1", NF4REG)
	}
	if NF4DIR != 2 {
		t.Errorf("NF4DIR = %d, want 2", NF4DIR)
	}
	if NF4NAMEDATTR != 9 {
		t.Errorf("NF4NAMEDATTR = %d, want 9", NF4NAMEDATTR)
	}
}
