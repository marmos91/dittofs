package pseudofs

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	pfs := New()
	if pfs == nil {
		t.Fatal("New() returned nil")
	}

	rootHandle := pfs.GetRootHandle()
	if rootHandle == nil {
		t.Fatal("root handle is nil")
	}

	// Root handle should have the pseudo-fs prefix
	if !IsPseudoFSHandle(rootHandle) {
		t.Errorf("root handle %q should be a pseudo-fs handle", string(rootHandle))
	}

	// Root handle should be "pseudofs:/"
	expected := "pseudofs:/"
	if string(rootHandle) != expected {
		t.Errorf("root handle = %q, want %q", string(rootHandle), expected)
	}

	// Should have exactly 1 node (root)
	if got := pfs.GetNodeCount(); got != 1 {
		t.Errorf("GetNodeCount() = %d, want 1", got)
	}
}

func TestIsPseudoFSHandle(t *testing.T) {
	tests := []struct {
		name   string
		handle []byte
		want   bool
	}{
		{"pseudo-fs root", []byte("pseudofs:/"), true},
		{"pseudo-fs path", []byte("pseudofs:/export"), true},
		{"pseudo-fs hashed", []byte("pseudofs:abc123"), true},
		{"real handle shareName:uuid", []byte("export:550e8400-e29b-41d4-a716-446655440000"), false},
		{"real handle bytes", []byte{0x01, 0x02, 0x03, 0x04}, false},
		{"empty handle", []byte{}, false},
		{"prefix only", []byte("pseudofs:"), true},
		{"nil handle", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPseudoFSHandle(tt.handle); got != tt.want {
				t.Errorf("IsPseudoFSHandle(%q) = %v, want %v", string(tt.handle), got, tt.want)
			}
		})
	}
}

func TestRebuildSingleShare(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export"})

	// Should have root + export = 2 nodes
	if got := pfs.GetNodeCount(); got != 2 {
		t.Errorf("GetNodeCount() = %d, want 2", got)
	}

	// Look up by root handle
	rootNode, ok := pfs.LookupByHandle(pfs.GetRootHandle())
	if !ok {
		t.Fatal("root node not found by handle")
	}

	// Root should have one child "export"
	exportNode, ok := pfs.LookupChild(rootNode, "export")
	if !ok {
		t.Fatal("export child not found")
	}

	if !exportNode.IsExport {
		t.Error("export node should have IsExport=true")
	}
	if exportNode.ShareName != "/export" {
		t.Errorf("export ShareName = %q, want %q", exportNode.ShareName, "/export")
	}
	if exportNode.Name != "export" {
		t.Errorf("export Name = %q, want %q", exportNode.Name, "export")
	}
	if exportNode.Path != "/export" {
		t.Errorf("export Path = %q, want %q", exportNode.Path, "/export")
	}
}

func TestRebuildNestedShare(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/data/archive"})

	// Should have root + "data" intermediate + "data/archive" export = 3 nodes
	if got := pfs.GetNodeCount(); got != 3 {
		t.Errorf("GetNodeCount() = %d, want 3", got)
	}

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())

	// Check "data" intermediate node
	dataNode, ok := pfs.LookupChild(rootNode, "data")
	if !ok {
		t.Fatal("data child not found")
	}
	if dataNode.IsExport {
		t.Error("data node should not be an export (it's an intermediate)")
	}
	if dataNode.Path != "/data" {
		t.Errorf("data Path = %q, want %q", dataNode.Path, "/data")
	}

	// Check "archive" export node
	archiveNode, ok := pfs.LookupChild(dataNode, "archive")
	if !ok {
		t.Fatal("archive child not found")
	}
	if !archiveNode.IsExport {
		t.Error("archive node should have IsExport=true")
	}
	if archiveNode.ShareName != "/data/archive" {
		t.Errorf("archive ShareName = %q, want %q", archiveNode.ShareName, "/data/archive")
	}
}

func TestRebuildMultipleShares(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export", "/data/archive", "/data/backup"})

	// root + export + data + data/archive + data/backup = 5
	if got := pfs.GetNodeCount(); got != 5 {
		t.Errorf("GetNodeCount() = %d, want 5", got)
	}

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())

	// "export" should be a direct child of root
	_, ok := pfs.LookupChild(rootNode, "export")
	if !ok {
		t.Error("export not found as child of root")
	}

	// "data" should be a direct child of root
	dataNode, ok := pfs.LookupChild(rootNode, "data")
	if !ok {
		t.Fatal("data not found as child of root")
	}

	// "data" should have two children: archive and backup
	children := pfs.ListChildren(dataNode)
	if len(children) != 2 {
		t.Fatalf("data has %d children, want 2", len(children))
	}
	if children[0].Name != "archive" {
		t.Errorf("first child = %q, want %q", children[0].Name, "archive")
	}
	if children[1].Name != "backup" {
		t.Errorf("second child = %q, want %q", children[1].Name, "backup")
	}
}

func TestRebuildPreservesRootHandle(t *testing.T) {
	pfs := New()
	originalRoot := make([]byte, len(pfs.GetRootHandle()))
	copy(originalRoot, pfs.GetRootHandle())

	// Rebuild multiple times
	pfs.Rebuild([]string{"/export"})
	pfs.Rebuild([]string{"/different"})
	pfs.Rebuild([]string{"/export", "/data"})

	newRoot := pfs.GetRootHandle()
	if string(originalRoot) != string(newRoot) {
		t.Errorf("root handle changed after rebuild: %q -> %q", string(originalRoot), string(newRoot))
	}
}

func TestLookupByHandle(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export"})

	// Look up root by handle
	rootNode, ok := pfs.LookupByHandle(pfs.GetRootHandle())
	if !ok {
		t.Fatal("root not found by handle")
	}
	if rootNode.Path != "/" {
		t.Errorf("root Path = %q, want %q", rootNode.Path, "/")
	}

	// Look up export by handle
	exportNode, ok := pfs.LookupChild(rootNode, "export")
	if !ok {
		t.Fatal("export child not found")
	}
	foundNode, ok := pfs.LookupByHandle(exportNode.Handle)
	if !ok {
		t.Fatal("export not found by handle")
	}
	if foundNode.Path != "/export" {
		t.Errorf("found node Path = %q, want %q", foundNode.Path, "/export")
	}

	// Look up nonexistent handle
	_, ok = pfs.LookupByHandle([]byte("pseudofs:/nonexistent"))
	if ok {
		t.Error("nonexistent handle should not be found")
	}
}

func TestLookupChild(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export"})

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())

	// Existing child
	_, ok := pfs.LookupChild(rootNode, "export")
	if !ok {
		t.Error("export child should be found")
	}

	// Nonexistent child
	_, ok = pfs.LookupChild(rootNode, "nonexistent")
	if ok {
		t.Error("nonexistent child should not be found")
	}

	// Nil parent
	_, ok = pfs.LookupChild(nil, "export")
	if ok {
		t.Error("nil parent should return false")
	}
}

func TestLookupParent(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/data/archive"})

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	dataNode, _ := pfs.LookupChild(rootNode, "data")
	archiveNode, _ := pfs.LookupChild(dataNode, "archive")

	// Archive's parent is data
	parent, ok := pfs.LookupParent(archiveNode)
	if !ok {
		t.Fatal("archive parent not found")
	}
	if parent.Path != "/data" {
		t.Errorf("archive parent Path = %q, want %q", parent.Path, "/data")
	}

	// Data's parent is root
	parent, ok = pfs.LookupParent(dataNode)
	if !ok {
		t.Fatal("data parent not found")
	}
	if parent.Path != "/" {
		t.Errorf("data parent Path = %q, want %q", parent.Path, "/")
	}

	// Root's parent is root itself
	parent, ok = pfs.LookupParent(rootNode)
	if !ok {
		t.Fatal("root parent not found")
	}
	if parent.Path != "/" {
		t.Errorf("root parent Path = %q, want %q", parent.Path, "/")
	}
	if parent != rootNode {
		t.Error("root's parent should be root itself")
	}

	// Nil node
	_, ok = pfs.LookupParent(nil)
	if ok {
		t.Error("nil node should return false")
	}
}

func TestListChildren(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/c_share", "/a_share", "/b_share"})

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	children := pfs.ListChildren(rootNode)

	if len(children) != 3 {
		t.Fatalf("ListChildren returned %d children, want 3", len(children))
	}

	// Should be sorted alphabetically
	expected := []string{"a_share", "b_share", "c_share"}
	for i, child := range children {
		if child.Name != expected[i] {
			t.Errorf("child[%d].Name = %q, want %q", i, child.Name, expected[i])
		}
	}

	// Nil node
	if got := pfs.ListChildren(nil); got != nil {
		t.Errorf("ListChildren(nil) = %v, want nil", got)
	}
}

func TestHandleStability(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export", "/data/archive"})

	// Capture handles
	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	exportNode, _ := pfs.LookupChild(rootNode, "export")
	dataNode, _ := pfs.LookupChild(rootNode, "data")
	archiveNode, _ := pfs.LookupChild(dataNode, "archive")

	exportHandle := make([]byte, len(exportNode.Handle))
	copy(exportHandle, exportNode.Handle)
	archiveHandle := make([]byte, len(archiveNode.Handle))
	copy(archiveHandle, archiveNode.Handle)

	// Rebuild with same shares
	pfs.Rebuild([]string{"/export", "/data/archive"})

	// Verify handles are the same
	rootNode2, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	exportNode2, _ := pfs.LookupChild(rootNode2, "export")
	dataNode2, _ := pfs.LookupChild(rootNode2, "data")
	archiveNode2, _ := pfs.LookupChild(dataNode2, "archive")

	if string(exportNode2.Handle) != string(exportHandle) {
		t.Errorf("export handle changed after rebuild: %q -> %q",
			string(exportHandle), string(exportNode2.Handle))
	}
	if string(archiveNode2.Handle) != string(archiveHandle) {
		t.Errorf("archive handle changed after rebuild: %q -> %q",
			string(archiveHandle), string(archiveNode2.Handle))
	}
}

func TestExportVsIntermediate(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/data/archive"})

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	dataNode, _ := pfs.LookupChild(rootNode, "data")
	archiveNode, _ := pfs.LookupChild(dataNode, "archive")

	if dataNode.IsExport {
		t.Error("intermediate 'data' node should not be an export")
	}
	if dataNode.ShareName != "" {
		t.Errorf("intermediate 'data' ShareName = %q, want empty", dataNode.ShareName)
	}

	if !archiveNode.IsExport {
		t.Error("'archive' node should be an export")
	}
	if archiveNode.ShareName != "/data/archive" {
		t.Errorf("'archive' ShareName = %q, want %q", archiveNode.ShareName, "/data/archive")
	}
}

func TestHandleFitsWithinMaxSize(t *testing.T) {
	pfs := New()

	// Create a share with a very long path that would exceed 128 bytes
	longPath := "/" + strings.Repeat("a", 200)
	pfs.Rebuild([]string{longPath})

	// Verify all handles fit within maxHandleSize
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()

	for p, h := range pfs.handles {
		if len(h) > maxHandleSize {
			t.Errorf("handle for %q has length %d, exceeds max %d", p, len(h), maxHandleSize)
		}
	}
}

func TestPseudoFSAttrSource(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export"})

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	exportNode, _ := pfs.LookupChild(rootNode, "export")

	// Test GetHandle
	if exportNode.GetHandle() == nil {
		t.Error("GetHandle() returned nil")
	}

	// Test GetFSID
	major, minor := exportNode.GetFSID()
	if major != 0 || minor != 1 {
		t.Errorf("GetFSID() = (%d, %d), want (0, 1)", major, minor)
	}

	// Test GetFileID
	if exportNode.GetFileID() == 0 {
		t.Error("GetFileID() should be non-zero for non-root nodes")
	}

	// Test GetType
	if got := exportNode.GetType(); got != 2 { // NF4DIR = 2
		t.Errorf("GetType() = %d, want 2 (NF4DIR)", got)
	}
}

func TestRebuildChangeID(t *testing.T) {
	pfs := New()

	initialChangeID := pfs.root.ChangeID

	pfs.Rebuild([]string{"/export"})
	if pfs.root.ChangeID != initialChangeID+1 {
		t.Errorf("ChangeID after first rebuild = %d, want %d",
			pfs.root.ChangeID, initialChangeID+1)
	}

	pfs.Rebuild([]string{"/export", "/data"})
	if pfs.root.ChangeID != initialChangeID+2 {
		t.Errorf("ChangeID after second rebuild = %d, want %d",
			pfs.root.ChangeID, initialChangeID+2)
	}
}

func TestRebuildClearsOldNodes(t *testing.T) {
	pfs := New()

	// First rebuild with /export
	pfs.Rebuild([]string{"/export"})
	if got := pfs.GetNodeCount(); got != 2 {
		t.Errorf("after first rebuild: GetNodeCount() = %d, want 2", got)
	}

	// Second rebuild with different shares
	pfs.Rebuild([]string{"/data/archive"})
	if got := pfs.GetNodeCount(); got != 3 {
		t.Errorf("after second rebuild: GetNodeCount() = %d, want 3 (root + data + archive)", got)
	}

	// /export should no longer exist
	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	_, ok := pfs.LookupChild(rootNode, "export")
	if ok {
		t.Error("/export should not exist after rebuild with different shares")
	}
}

func TestRebuildEmptyShares(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{})

	// Only root should exist
	if got := pfs.GetNodeCount(); got != 1 {
		t.Errorf("GetNodeCount() = %d, want 1 (root only)", got)
	}
}

func TestFileIDStability(t *testing.T) {
	pfs := New()
	pfs.Rebuild([]string{"/export"})

	rootNode, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	exportNode, _ := pfs.LookupChild(rootNode, "export")
	originalFileID := exportNode.GetFileID()

	// Rebuild with same shares
	pfs.Rebuild([]string{"/export"})

	rootNode2, _ := pfs.LookupByHandle(pfs.GetRootHandle())
	exportNode2, _ := pfs.LookupChild(rootNode2, "export")

	if exportNode2.GetFileID() != originalFileID {
		t.Errorf("FileID changed after rebuild: %d -> %d", originalFileID, exportNode2.GetFileID())
	}
}
