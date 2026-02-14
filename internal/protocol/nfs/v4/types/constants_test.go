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
