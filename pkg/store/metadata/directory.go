package metadata

// ReadDirPage represents one page of directory entries returned by ReadDirectory.
//
// This structure supports paginated directory reading, which is essential for:
//   - Large directories that don't fit in a single response
//   - Memory-efficient directory traversal
//   - Protocol compliance (NFS, SMB, S3 all use pagination)
//   - Incremental UI updates (show entries as they arrive)
//
// Pagination Flow:
//
//	// Start reading directory
//	page, err := repo.ReadDirectory(ctx, dirHandle, "", 8192)
//	if err != nil {
//	    return err
//	}
//
//	// Process first page
//	for _, entry := range page.Entries {
//	    fmt.Printf("%s\n", entry.Name)
//	}
//
//	// Continue if more pages exist
//	for page.HasMore {
//	    page, err = repo.ReadDirectory(ctx, dirHandle, page.NextToken, 8192)
//	    if err != nil {
//	        return err
//	    }
//	    for _, entry := range page.Entries {
//	        fmt.Printf("%s\n", entry.Name)
//	    }
//	}
//
// Special Entries:
//
// The implementation may include "." (current directory) and ".." (parent directory)
// as the first entries when reading from the beginning (token=""). These are not
// real directory entries but virtual entries required by POSIX semantics.
//
// Empty Pages:
//
// An empty Entries slice with HasMore=true is valid and indicates that the
// implementation couldn't fit even one entry in the size limit. The caller
// should increase maxBytes and retry with the same token.
//
// Token Invalidation:
//
// Tokens may become invalid if:
//   - The directory is modified between pagination calls
//   - Too much time passes between calls (timeout)
//   - The server restarts
//
// When a token becomes invalid, ReadDirectory returns ErrInvalidArgument.
// Clients should restart pagination from the beginning (token="").
//
// Thread Safety:
//
// ReadDirPage instances are immutable after creation and safe for concurrent
// reading. However, pagination tokens are tied to a specific point-in-time
// view of the directory. Concurrent modifications may cause:
//   - Entries to be skipped (if renamed/moved before being read)
//   - Entries to appear twice (if renamed into already-read portion)
//   - Tokens to become invalid
//
// For consistent directory snapshots, implementations should use appropriate
// locking or versioning strategies.
type ReadDirPage struct {
	// Entries contains the directory entries for this page.
	//
	// The order of entries is implementation-specific but should be stable
	// (consistent across pagination calls for the same directory state).
	// Common ordering strategies:
	//   - Alphabetical by name (most user-friendly)
	//   - Inode/ID order (most efficient for filesystem iteration)
	//   - Insertion order (simplest for some implementations)
	//
	// May be empty if:
	//   - The directory is empty (and HasMore=false)
	//   - No entries fit within the size limit (and HasMore=true, rare)
	//
	// When token="" (first page), may include special entries "." and ".."
	// as the first two entries, following POSIX conventions.
	Entries []DirEntry

	// NextToken is the pagination token to use for retrieving the next page.
	//
	// Token Semantics:
	//   - Empty string (""): No more pages, pagination complete
	//   - Non-empty: Pass this value to ReadDirectory to get the next page
	//
	// Token Properties:
	//   - Opaque: Clients must treat as an opaque string
	//   - Ephemeral: May expire after some time or server restart
	//   - Stateless: Should not require server-side session state (preferred)
	//   - URL-safe: May contain only characters safe for URLs/JSON (recommended)
	//
	// Token Format Examples (implementation-specific):
	//   - Offset-based: "0", "100", "200" (simple but fragile under modifications)
	//   - Name-based: "file123.txt" (resume after this filename)
	//   - Cursor-based: "cursor:YXJyYXk=" (base64-encoded state)
	//   - Composite: "v1:ts:1234567890:name:file.txt" (versioned, structured)
	//
	// Token Validation:
	//   - Implementations must validate tokens and return ErrInvalidArgument
	//     for invalid, expired, or corrupted tokens
	//   - Tokens from one directory should not work for another directory
	//   - Tokens should include integrity checks (HMAC, checksum) if security matters
	//
	// Example token generation (simple offset):
	//
	//	if hasMoreEntries {
	//	    page.NextToken = strconv.FormatUint(nextOffset, 10)
	//	} else {
	//	    page.NextToken = ""
	//	}
	//
	// Example token generation (cursor with integrity):
	//
	//	cursor := Cursor{DirID: dirID, Offset: offset, Timestamp: time.Now().Unix()}
	//	encoded := base64.URLEncoding.EncodeToString(json.Marshal(cursor))
	//	hmac := computeHMAC(encoded, secret)
	//	page.NextToken = encoded + ":" + hmac
	NextToken string

	// HasMore indicates whether more entries are available after this page.
	//
	// This is a convenience field equivalent to (NextToken != "").
	// It allows for more readable code:
	//
	//	// Using HasMore (clearer intent)
	//	for page.HasMore {
	//	    page, err = repo.ReadDirectory(ctx, dirHandle, page.NextToken, size)
	//	}
	//
	//	// Using NextToken (also valid)
	//	for page.NextToken != "" {
	//	    page, err = repo.ReadDirectory(ctx, dirHandle, page.NextToken, size)
	//	}
	//
	// Invariant: HasMore == (NextToken != "")
	//
	// Implementations should ensure this invariant:
	//   - If NextToken is empty, HasMore must be false
	//   - If NextToken is non-empty, HasMore must be true
	HasMore bool
}

// DirEntry represents a single entry in a directory listing.
//
// This is a minimal structure containing only the information needed for
// directory iteration. For full attributes, clients use Lookup or GetFile
// on the entry's ID.
type DirEntry struct {
	// ID is the unique identifier for the file/directory
	// This typically maps to an inode number in Unix systems
	ID uint64

	// Name is the filename
	// Does not include the parent path
	Name string

	// Handle is the file handle for this entry
	// This avoids expensive Lookup() calls in READDIRPLUS
	// Implementations MUST populate this field for performance
	Handle FileHandle

	// Attr contains the file attributes (optional, for READDIRPLUS optimization)
	// If nil, READDIRPLUS will call GetFile() to retrieve attributes
	// If populated, READDIRPLUS can avoid per-entry GetFile() calls
	Attr *FileAttr
}
