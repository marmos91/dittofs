// Package pseudofs implements the NFSv4 pseudo-filesystem virtual namespace.
//
// The pseudo-filesystem presents all runtime shares under a unified root
// directory tree. When a client connects via NFSv4, it navigates the pseudo-fs
// to reach export junction points, which then delegate to real metadata stores.
//
// Pseudo-fs handles are distinguishable from real file handles by their
// "pseudofs:" prefix. This allows the COMPOUND dispatcher to route operations
// to either the pseudo-fs or the real metadata service.
//
// The pseudo-fs supports dynamic rebuilds when shares are added or removed,
// preserving handle stability for nodes that survive the rebuild.
package pseudofs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	v4types "github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// pseudoFSHandlePrefix is the prefix used to distinguish pseudo-fs handles
// from real file handles. All pseudo-fs handles start with this prefix.
const pseudoFSHandlePrefix = "pseudofs:"

// maxHandleSize is the maximum NFSv4 file handle size in bytes.
const maxHandleSize = v4types.NFS4_FHSIZE

// PseudoNode represents a single node in the pseudo-filesystem tree.
// Each node corresponds to a directory in the virtual namespace.
type PseudoNode struct {
	// Name is the node name (e.g., "export", "data").
	Name string

	// Path is the full path from root (e.g., "/export", "/data/archive").
	Path string

	// Handle is the unique opaque handle for this node.
	Handle []byte

	// Children maps child names to child nodes.
	Children map[string]*PseudoNode

	// IsExport is true if this node is a junction point to a real share.
	IsExport bool

	// ShareName is the share name if IsExport is true.
	// Used for routing operations to the correct metadata service.
	ShareName string

	// Parent is a reference to the parent node. Root's parent is itself.
	Parent *PseudoNode

	// ChangeID is a monotonic counter bumped on child changes.
	ChangeID uint64

	// FileID is a stable unique file identifier for GETATTR.
	FileID uint64
}

// GetHandle returns the file handle for this node.
// Implements attrs.PseudoFSAttrSource.
func (n *PseudoNode) GetHandle() []byte {
	return n.Handle
}

// GetFSID returns the filesystem ID (major, minor) for this node.
// All pseudo-fs nodes share FSID (0, 1) per research recommendation.
// Implements attrs.PseudoFSAttrSource.
func (n *PseudoNode) GetFSID() (uint64, uint64) {
	return 0, 1
}

// GetFileID returns the unique file identifier for this node.
// Implements attrs.PseudoFSAttrSource.
func (n *PseudoNode) GetFileID() uint64 {
	return n.FileID
}

// GetChangeID returns the change attribute value for this node.
// Implements attrs.PseudoFSAttrSource.
func (n *PseudoNode) GetChangeID() uint64 {
	return n.ChangeID
}

// GetType returns NF4DIR since all pseudo-fs nodes are directories.
// Implements attrs.PseudoFSAttrSource.
func (n *PseudoNode) GetType() uint32 {
	return v4types.NF4DIR
}

// PseudoFS is the virtual namespace tree for NFSv4.
// It presents all shares under a unified root and supports dynamic rebuilds.
//
// Thread safety: All public methods are safe for concurrent use.
type PseudoFS struct {
	mu       sync.RWMutex
	root     *PseudoNode
	handles  map[string][]byte      // path -> handle mapping
	byHandle map[string]*PseudoNode // hex(handle) -> node (reverse lookup)
	nextID   uint64                 // monotonic counter for handle/fileID generation
}

// New creates a new empty PseudoFS with a root node at "/".
func New() *PseudoFS {
	pfs := &PseudoFS{
		handles:  make(map[string][]byte),
		byHandle: make(map[string]*PseudoNode),
		nextID:   1, // Start at 1, reserve 0
	}

	// Create root node
	rootHandle := makeHandle("/")
	pfs.root = &PseudoNode{
		Name:     "",
		Path:     "/",
		Handle:   rootHandle,
		Children: make(map[string]*PseudoNode),
		FileID:   0, // Root gets FileID 0
	}
	// Root's parent is itself per NFSv4 spec
	pfs.root.Parent = pfs.root

	// Register root in lookup maps
	pfs.handles["/"] = rootHandle
	pfs.byHandle[hex.EncodeToString(rootHandle)] = pfs.root

	return pfs
}

// IsPseudoFSHandle returns true if the given handle belongs to the pseudo-fs.
// Pseudo-fs handles are distinguished by the "pseudofs:" prefix.
func IsPseudoFSHandle(handle []byte) bool {
	return len(handle) >= len(pseudoFSHandlePrefix) &&
		string(handle[:len(pseudoFSHandlePrefix)]) == pseudoFSHandlePrefix
}

// GetRootHandle returns the handle for the pseudo-fs root node.
func (pfs *PseudoFS) GetRootHandle() []byte {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	return pfs.root.Handle
}

// LookupByHandle looks up a node by its handle.
// Returns the node and true if found, nil and false otherwise.
func (pfs *PseudoFS) LookupByHandle(handle []byte) (*PseudoNode, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	key := hex.EncodeToString(handle)
	node, ok := pfs.byHandle[key]
	return node, ok
}

// LookupChild looks up a child by name in the parent's children map.
// Returns the child node and true if found, nil and false otherwise.
func (pfs *PseudoFS) LookupChild(parent *PseudoNode, name string) (*PseudoNode, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	if parent == nil || parent.Children == nil {
		return nil, false
	}
	child, ok := parent.Children[name]
	return child, ok
}

// LookupParent returns the parent node of the given node.
// Root's parent is root itself per NFSv4 spec (LOOKUPP on root returns root).
// Returns the parent and true if found, nil and false if node is nil.
func (pfs *PseudoFS) LookupParent(node *PseudoNode) (*PseudoNode, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	if node == nil {
		return nil, false
	}
	return node.Parent, true
}

// Rebuild rebuilds the pseudo-fs tree from a list of share paths.
//
// The rebuild process:
//  1. Preserves the root node and its handle for stability
//  2. Clears all non-root nodes
//  3. Creates intermediate directory nodes and export junction nodes
//  4. Reuses handles for nodes that existed before the rebuild
//  5. Bumps the root's ChangeID
//
// Example: shares ["/data/archive", "/export"] creates:
//   - "/" (root)
//   - "/data" (intermediate directory)
//   - "/data/archive" (export junction, IsExport=true)
//   - "/export" (export junction, IsExport=true)
func (pfs *PseudoFS) Rebuild(shares []string) {
	pfs.mu.Lock()
	defer pfs.mu.Unlock()

	// Save old handles for stability
	oldHandles := make(map[string][]byte)
	for p, h := range pfs.handles {
		oldHandles[p] = h
	}

	// Save old FileIDs for stability
	oldFileIDs := make(map[string]uint64)
	for _, node := range pfs.byHandle {
		oldFileIDs[node.Path] = node.FileID
	}

	// Clear all non-root nodes from lookup maps
	rootHandleKey := hex.EncodeToString(pfs.root.Handle)
	pfs.handles = map[string][]byte{
		"/": pfs.root.Handle,
	}
	pfs.byHandle = map[string]*PseudoNode{
		rootHandleKey: pfs.root,
	}

	// Clear root's children
	pfs.root.Children = make(map[string]*PseudoNode)

	// Build tree from share paths
	for _, sharePath := range shares {
		// Normalize the share path
		sharePath = path.Clean(sharePath)
		if !strings.HasPrefix(sharePath, "/") {
			sharePath = "/" + sharePath
		}

		// Split path into components
		parts := strings.Split(strings.TrimPrefix(sharePath, "/"), "/")
		current := pfs.root

		for i, part := range parts {
			if part == "" {
				continue
			}

			child, exists := current.Children[part]
			if !exists {
				// Build full path for this node
				childPath := "/" + strings.Join(parts[:i+1], "/")

				// Determine if this is the export (last component)
				isExport := (i == len(parts)-1)

				// Reuse old handle if available, otherwise generate new one
				var handle []byte
				if old, ok := oldHandles[childPath]; ok {
					handle = old
				} else {
					handle = makeHandle(childPath)
				}

				// Reuse old FileID if available, otherwise generate new one
				var fileID uint64
				if old, ok := oldFileIDs[childPath]; ok {
					fileID = old
				} else {
					fileID = atomic.AddUint64(&pfs.nextID, 1)
				}

				child = &PseudoNode{
					Name:     part,
					Path:     childPath,
					Handle:   handle,
					Children: make(map[string]*PseudoNode),
					IsExport: isExport,
					Parent:   current,
					FileID:   fileID,
				}

				if isExport {
					child.ShareName = sharePath
				}

				current.Children[part] = child

				// Register in lookup maps
				pfs.handles[childPath] = handle
				pfs.byHandle[hex.EncodeToString(handle)] = child
			} else {
				// Node already exists (shared intermediate path).
				// If this is the final component, mark as export.
				if i == len(parts)-1 {
					child.IsExport = true
					child.ShareName = sharePath
				}
			}

			current = child
		}
	}

	// Bump root's ChangeID to signal tree modification
	pfs.root.ChangeID++
}

// ListChildren returns a sorted list of children for the given node.
func (pfs *PseudoFS) ListChildren(node *PseudoNode) []*PseudoNode {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()

	if node == nil || node.Children == nil {
		return nil
	}

	// Collect children and sort by name
	children := make([]*PseudoNode, 0, len(node.Children))
	for _, child := range node.Children {
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Name < children[j].Name
	})

	return children
}

// FindJunction looks up the pseudo-fs junction node for a given share name.
// Returns the junction node and true if found, nil and false if no junction
// exists for the share.
//
// This is used by LOOKUPP to cross back from real-FS to pseudo-fs when
// navigating above the share root.
func (pfs *PseudoFS) FindJunction(shareName string) (*PseudoNode, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()

	for _, node := range pfs.byHandle {
		if node.IsExport && node.ShareName == shareName {
			return node, true
		}
	}
	return nil, false
}

// GetNodeCount returns the total number of nodes in the pseudo-fs tree.
// Primarily used for testing and debugging.
func (pfs *PseudoFS) GetNodeCount() int {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	return len(pfs.byHandle)
}

// makeHandle generates a pseudo-fs handle for the given path.
//
// Handle format: "pseudofs:" + path
// If the total handle exceeds maxHandleSize (128 bytes), the path portion
// is hashed with SHA-256 and the first 16 bytes of the hex-encoded hash
// are used instead.
func makeHandle(nodePath string) []byte {
	candidate := pseudoFSHandlePrefix + nodePath
	if len(candidate) <= maxHandleSize {
		return []byte(candidate)
	}

	// Path too long -- hash it
	hash := sha256.Sum256([]byte(nodePath))
	shortHash := hex.EncodeToString(hash[:16]) // 32 hex chars
	return []byte(fmt.Sprintf("%s%s", pseudoFSHandlePrefix, shortHash))
}
