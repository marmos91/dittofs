package metadata

import (
	"context"
	"path"
	"sort"
	"strings"

	metaerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ============================================================================
// Unified xattr resolver
// ============================================================================
//
// This file implements a single extended-attribute (xattr) namespace over the
// TWO physical backings DittoFS already maintains, so every protocol family
// (SMB EA, SMB named streams, NFSv4.2 GETXATTR/SETXATTR/…) reads and writes the
// SAME data and therefore sees each other. No protocol handler changes are
// required: the resolver only reads the backings the SMB layer already writes.
//
// The two backings are:
//
//  1. Inline K/V — FileAttr.EAs (small values, <= XattrInlineMaxBytes). This is
//     the default home for new xattrs. Names resolve case-insensitively with
//     set-casing preservation, reusing LookupEA / ApplyEAMutations / findEAKey.
//     Storage rides the existing EA JSON/JSONB column (see FileAttr.EAs).
//
//  2. Named-stream entities — colon-named "<base>:<name>" child Files of the
//     base file's PARENT directory (block-backed, full file identity). These
//     are enumerated with a colon-prefix scan over ListChildren, mirroring
//     internal/adapter/smb/handlers/query_info.go:buildFileStreamInformation.
//     A stream's content is the xattr value and is read through the block store
//     (engine.Store.ReadAt) via an injected StreamContentReader — the metadata
//     layer never imports the block engine directly.
//
// Precedence: STREAM-ENTITY-WINS-ELSE-INLINE. When both a same-named stream
// child and an inline EA exist, Get/List surface the stream's content/name.
//
// SetXattr: a value <= XattrInlineMaxBytes is written inline via
// ApplyEAMutations. A larger value returns the sentinel ErrXattrTooLarge (the
// NFS adapter maps it to NFS4ERR_XATTR2BIG). Spilling oversized values into a
// freshly-created named-stream entity from the store layer requires
// block-store coordination that does not exist at this tier; it is a
// documented PR2 follow-up (issue #1285). The READ path is complete in PR1:
// List/Get already surface stream-backed xattrs created by SMB so
// cross-protocol parity holds for existing streams.
//
// All resolution logic lives here as free functions over the Files interface
// so every backend (memory / badger / postgres, store + transaction receivers)
// shares one implementation; the per-backend Files methods are thin
// delegations.

// XattrInlineMaxBytes is the maximum value size stored inline in FileAttr.EAs.
// Values larger than this exceed the inline backing and SetXattr returns
// ErrXattrTooLarge (mapped to NFS4ERR_XATTR2BIG by the NFS adapter).
const XattrInlineMaxBytes = 64 * 1024

// ErrXattrTooLarge is returned by SetXattr (and ResolveSetXattr) when the value
// exceeds XattrInlineMaxBytes and therefore cannot be stored inline. The NFS
// adapter maps it to NFS4ERR_XATTR2BIG.
var ErrXattrTooLarge = &StoreError{
	Code:    metaerrors.ErrInvalidArgument,
	Message: "xattr value too large for inline storage",
}

// StreamContentReader reads the full content of a named-stream child File so
// the resolver can surface a stream-backed xattr value. It is injected by the
// caller that has block-store access (the runtime/adapter layer); the metadata
// layer stays block-engine-agnostic. A nil reader disables stream-content
// reads: stream NAMES are still enumerated by ListXattr, but GetXattr on a
// stream-backed name reports it as not found (callers without a reader cannot
// materialise the value).
type StreamContentReader func(ctx context.Context, streamHandle FileHandle, attr *FileAttr) ([]byte, error)

// streamSeparator separates a base file name from its stream name in the colon
// child-naming convention ("<base>:<stream>"). Mirrors the SMB ADS layout.
const streamSeparator = ":"

// streamChildPrefix returns the "<base>:" prefix used to scan a parent
// directory's children for the base file's named streams.
func streamChildPrefix(baseName string) string {
	return baseName + streamSeparator
}

// streamNameFromChild extracts the stream-name portion of a colon child
// ("<base>:<stream>" -> "<stream>") when it matches the base, case-insensitively.
// Returns ("", false) when childName is not a stream of baseName.
func streamNameFromChild(baseName, childName string) (string, bool) {
	prefix := streamChildPrefix(baseName)
	if len(childName) <= len(prefix) {
		return "", false
	}
	if !strings.EqualFold(childName[:len(prefix)], prefix) {
		return "", false
	}
	return childName[len(prefix):], true
}

// baseNameForHandle returns the file's own base name (final path component) and
// its parent handle, used to scan for named-stream siblings. A file with no
// recoverable parent (root, or a backend that does not track the parent) has no
// named streams; (nil parent, "", nil) is returned and stream resolution is
// skipped gracefully.
func baseNameForHandle(ctx context.Context, files Files, handle FileHandle, file *File) (FileHandle, string, error) {
	parent, err := files.GetParent(ctx, handle)
	if err != nil {
		// No parent (root) or backend without parent tracking: no streams.
		if IsNotFoundError(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	base := path.Base(file.Path)
	if base == "." || base == "/" || base == "" {
		return nil, "", nil
	}
	return parent, base, nil
}

// findStreamChild scans the parent directory for a "<base>:<name>" child whose
// stream name matches name case-insensitively, returning its handle, attrs, and
// whether a match was found. parent may be nil (no streams).
func findStreamChild(ctx context.Context, files Files, parent FileHandle, baseName, name string) (FileHandle, *FileAttr, bool, error) {
	if parent == nil || baseName == "" {
		return nil, nil, false, nil
	}
	cursor := ""
	for {
		entries, next, err := files.ListChildren(ctx, parent, cursor, 0)
		if err != nil {
			return nil, nil, false, err
		}
		for i := range entries {
			streamName, ok := streamNameFromChild(baseName, entries[i].Name)
			if !ok {
				continue
			}
			if strings.EqualFold(streamName, name) {
				return entries[i].Handle, entries[i].Attr, true, nil
			}
		}
		if next == "" {
			return nil, nil, false, nil
		}
		cursor = next
	}
}

// ============================================================================
// Resolver free functions (shared by all backends)
// ============================================================================

// ResolveGetXattr resolves a single xattr name against both backings with
// stream-entity-wins precedence. A stream-backed value is materialised through
// reader; when reader is nil, a stream-backed name reports not-found because the
// value cannot be read without block-store access (the inline backing is still
// consulted). Returns (value, found, error).
func ResolveGetXattr(ctx context.Context, files Files, handle FileHandle, name string, reader StreamContentReader) ([]byte, bool, error) {
	file, err := files.GetFile(ctx, handle)
	if err != nil {
		return nil, false, err
	}

	// Stream-entity-wins: look for a named-stream child first.
	parent, baseName, err := baseNameForHandle(ctx, files, handle, file)
	if err != nil {
		return nil, false, err
	}
	streamHandle, streamAttr, ok, err := findStreamChild(ctx, files, parent, baseName, name)
	if err != nil {
		return nil, false, err
	}
	if ok {
		if reader == nil {
			// Stream exists but no block-store reader is wired at this tier.
			return nil, false, nil
		}
		val, rerr := reader(ctx, streamHandle, streamAttr)
		if rerr != nil {
			return nil, false, rerr
		}
		return val, true, nil
	}

	// Fall back to the inline EA backing.
	if val, found := file.LookupEA(name); found {
		// Return a defensive copy so callers cannot mutate the (already
		// deep-copied) GetFile result's backing array in surprising ways.
		out := make([]byte, len(val))
		copy(out, val)
		return out, true, nil
	}
	return nil, false, nil
}

// ResolveSetXattr writes an xattr value into the inline backing when it fits
// (<= XattrInlineMaxBytes), reusing ApplyEAMutations for case-insensitive,
// casing-preserving upsert. Oversized values return ErrXattrTooLarge (PR1 does
// not spill to a named-stream entity; see file header / issue #1285).
func ResolveSetXattr(ctx context.Context, files Files, handle FileHandle, name string, value []byte) error {
	if len(value) > XattrInlineMaxBytes {
		return ErrXattrTooLarge
	}
	file, err := files.GetFile(ctx, handle)
	if err != nil {
		return err
	}
	file.ApplyEAMutations([]EAMutation{{Name: name, Value: value}})
	return files.PutFile(ctx, file)
}

// ResolveRemoveXattr removes an xattr from the inline backing. Removing a name
// that is only stream-backed (or absent) returns ErrNotFound: PR1 does not
// delete named-stream entities from the store layer (that is a CREATE/DELETE
// of a full file entity, owned by the protocol layer). A name present in BOTH
// backings removes only the inline copy (the stream entity is untouched), which
// then makes the stream copy visible per the stream-wins precedence.
func ResolveRemoveXattr(ctx context.Context, files Files, handle FileHandle, name string) error {
	file, err := files.GetFile(ctx, handle)
	if err != nil {
		return err
	}
	if _, found := file.LookupEA(name); !found {
		return &StoreError{Code: metaerrors.ErrNotFound, Message: "xattr not found"}
	}
	file.ApplyEAMutations([]EAMutation{{Name: name, Delete: true}})
	return files.PutFile(ctx, file)
}

// ResolveListXattr returns every xattr name on the file, merged from both
// backings and de-duplicated case-insensitively (a name present in both backings
// appears once, in the stream's casing per the stream-wins precedence). Names
// are returned sorted for a stable, deterministic ordering across backends.
func ResolveListXattr(ctx context.Context, files Files, handle FileHandle) ([]string, error) {
	file, err := files.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}

	// Collect names case-insensitively, preferring stream casing on collision.
	type entry struct {
		name     string
		isStream bool
	}
	seen := make(map[string]entry)
	add := func(name string, isStream bool) {
		key := strings.ToLower(name)
		if existing, ok := seen[key]; ok {
			// Stream wins the casing/identity on collision.
			if isStream && !existing.isStream {
				seen[key] = entry{name: name, isStream: true}
			}
			return
		}
		seen[key] = entry{name: name, isStream: isStream}
	}

	// Stream-backed names from the parent directory scan.
	parent, baseName, err := baseNameForHandle(ctx, files, handle, file)
	if err != nil {
		return nil, err
	}
	if parent != nil && baseName != "" {
		cursor := ""
		for {
			entries, next, lerr := files.ListChildren(ctx, parent, cursor, 0)
			if lerr != nil {
				return nil, lerr
			}
			for i := range entries {
				if streamName, ok := streamNameFromChild(baseName, entries[i].Name); ok {
					add(streamName, true)
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}

	// Inline EA names.
	for k := range file.EAs {
		add(k, false)
	}

	names := make([]string, 0, len(seen))
	for _, e := range seen {
		names = append(names, e.name)
	}
	sort.Strings(names)
	return names, nil
}
