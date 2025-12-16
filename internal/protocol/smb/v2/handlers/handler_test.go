package handlers

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/session"
)

// =============================================================================
// Handler Creation Tests
// =============================================================================

func TestNewHandler(t *testing.T) {
	t.Run("CreatesValidHandler", func(t *testing.T) {
		h := NewHandler()

		if h == nil {
			t.Fatal("NewHandler() returned nil")
		}
	})

	t.Run("InitializesStartTime", func(t *testing.T) {
		before := time.Now()
		h := NewHandler()
		after := time.Now()

		if h.StartTime.Before(before) || h.StartTime.After(after) {
			t.Error("StartTime should be between before and after creation")
		}
	})

	t.Run("GeneratesServerGUID", func(t *testing.T) {
		h := NewHandler()

		// Check that GUID is not all zeros
		allZeros := true
		for _, b := range h.ServerGUID {
			if b != 0 {
				allZeros = false
				break
			}
		}

		if allZeros {
			t.Error("ServerGUID should not be all zeros")
		}
	})

	t.Run("GeneratesUniqueServerGUIDs", func(t *testing.T) {
		h1 := NewHandler()
		h2 := NewHandler()

		if h1.ServerGUID == h2.ServerGUID {
			t.Error("Two handlers should have different ServerGUIDs")
		}
	})

	t.Run("InitializesDefaultSizes", func(t *testing.T) {
		h := NewHandler()

		if h.MaxTransactSize == 0 {
			t.Error("MaxTransactSize should not be zero")
		}
		if h.MaxReadSize == 0 {
			t.Error("MaxReadSize should not be zero")
		}
		if h.MaxWriteSize == 0 {
			t.Error("MaxWriteSize should not be zero")
		}
	})
}

// =============================================================================
// Session Management Tests
// =============================================================================

func TestGenerateSessionID(t *testing.T) {
	t.Run("GeneratesUniqueIDs", func(t *testing.T) {
		h := NewHandler()

		ids := make(map[uint64]bool)
		for i := 0; i < 1000; i++ {
			id := h.GenerateSessionID()
			if ids[id] {
				t.Errorf("Duplicate session ID generated: %d", id)
			}
			ids[id] = true
		}
	})

	t.Run("GeneratesIncreasingIDs", func(t *testing.T) {
		h := NewHandler()

		prev := h.GenerateSessionID()
		for i := 0; i < 100; i++ {
			current := h.GenerateSessionID()
			if current <= prev {
				t.Errorf("IDs should be increasing: %d <= %d", current, prev)
			}
			prev = current
		}
	})

	t.Run("StartsFrom1", func(t *testing.T) {
		h := NewHandler()

		// First ID should be 2 (started at 1, then Add(1))
		id := h.GenerateSessionID()
		if id < 1 {
			t.Errorf("First session ID should be >= 1, got %d", id)
		}
	})
}

func TestSessionStorage(t *testing.T) {
	t.Run("CreateAndRetrieve", func(t *testing.T) {
		h := NewHandler()

		// Create session using the new API
		sess := h.CreateSession("127.0.0.1:12345", true, "guest", "")

		retrieved, ok := h.GetSession(sess.SessionID)
		if !ok {
			t.Fatal("Session not found")
		}

		if retrieved.SessionID != sess.SessionID {
			t.Errorf("SessionID mismatch: %d != %d", retrieved.SessionID, sess.SessionID)
		}
		if retrieved.IsGuest != sess.IsGuest {
			t.Errorf("IsGuest mismatch")
		}
		if retrieved.Username != sess.Username {
			t.Errorf("Username mismatch")
		}
	})

	t.Run("GetNonexistentSession", func(t *testing.T) {
		h := NewHandler()

		sess, ok := h.GetSession(99999)
		if ok {
			t.Error("Should not find nonexistent session")
		}
		if sess != nil {
			t.Error("Session should be nil for nonexistent ID")
		}
	})

	t.Run("DeleteSession", func(t *testing.T) {
		h := NewHandler()

		sess := h.CreateSession("127.0.0.1:12345", true, "guest", "")

		// Verify it exists
		_, ok := h.GetSession(sess.SessionID)
		if !ok {
			t.Fatal("Session should exist")
		}

		// Delete it
		h.DeleteSession(sess.SessionID)

		// Verify it's gone
		_, ok = h.GetSession(sess.SessionID)
		if ok {
			t.Error("Session should be deleted")
		}
	})

	t.Run("OverwriteSession", func(t *testing.T) {
		h := NewHandler()

		// Create session and store directly with specific ID
		sess1 := session.NewSession(1, "client", true, "user1", "")
		h.SessionManager.StoreSession(sess1)

		sess2 := session.NewSession(1, "client", true, "user2", "")
		h.SessionManager.StoreSession(sess2)

		retrieved, _ := h.GetSession(1)
		if retrieved.Username != "user2" {
			t.Error("Session should be overwritten")
		}
	})
}

// =============================================================================
// Pending Auth Management Tests
// =============================================================================

func TestPendingAuthStorage(t *testing.T) {
	t.Run("StoreAndRetrieve", func(t *testing.T) {
		h := NewHandler()

		pending := &PendingAuth{
			SessionID:  1,
			ClientAddr: "127.0.0.1:12345",
			CreatedAt:  time.Now(),
		}

		h.StorePendingAuth(pending)

		retrieved, ok := h.GetPendingAuth(1)
		if !ok {
			t.Fatal("PendingAuth not found")
		}

		if retrieved.SessionID != pending.SessionID {
			t.Errorf("SessionID mismatch")
		}
		if retrieved.ClientAddr != pending.ClientAddr {
			t.Errorf("ClientAddr mismatch")
		}
	})

	t.Run("GetNonexistentPendingAuth", func(t *testing.T) {
		h := NewHandler()

		pending, ok := h.GetPendingAuth(99999)
		if ok {
			t.Error("Should not find nonexistent pending auth")
		}
		if pending != nil {
			t.Error("PendingAuth should be nil")
		}
	})

	t.Run("DeletePendingAuth", func(t *testing.T) {
		h := NewHandler()

		pending := &PendingAuth{SessionID: 1}
		h.StorePendingAuth(pending)

		h.DeletePendingAuth(1)

		_, ok := h.GetPendingAuth(1)
		if ok {
			t.Error("PendingAuth should be deleted")
		}
	})
}

// =============================================================================
// Tree Connection Management Tests
// =============================================================================

func TestGenerateTreeID(t *testing.T) {
	t.Run("GeneratesUniqueIDs", func(t *testing.T) {
		h := NewHandler()

		ids := make(map[uint32]bool)
		for i := 0; i < 1000; i++ {
			id := h.GenerateTreeID()
			if ids[id] {
				t.Errorf("Duplicate tree ID generated: %d", id)
			}
			ids[id] = true
		}
	})
}

func TestTreeStorage(t *testing.T) {
	t.Run("StoreAndRetrieve", func(t *testing.T) {
		h := NewHandler()

		tree := &TreeConnection{
			TreeID:    1,
			SessionID: 100,
			ShareName: "export",
			ShareType: 1,
			CreatedAt: time.Now(),
		}

		h.StoreTree(tree)

		retrieved, ok := h.GetTree(1)
		if !ok {
			t.Fatal("Tree not found")
		}

		if retrieved.ShareName != tree.ShareName {
			t.Errorf("ShareName mismatch")
		}
	})

	t.Run("DeleteTree", func(t *testing.T) {
		h := NewHandler()

		tree := &TreeConnection{TreeID: 1}
		h.StoreTree(tree)

		h.DeleteTree(1)

		_, ok := h.GetTree(1)
		if ok {
			t.Error("Tree should be deleted")
		}
	})
}

// =============================================================================
// File Handle Management Tests
// =============================================================================

func TestGenerateFileID(t *testing.T) {
	t.Run("GeneratesValidFileID", func(t *testing.T) {
		h := NewHandler()

		fileID := h.GenerateFileID()

		// FileID should be 16 bytes
		if len(fileID) != 16 {
			t.Errorf("FileID should be 16 bytes, got %d", len(fileID))
		}
	})

	t.Run("GeneratesUniqueFileIDs", func(t *testing.T) {
		h := NewHandler()

		ids := make(map[string]bool)
		for i := 0; i < 100; i++ {
			id := h.GenerateFileID()
			key := string(id[:])
			if ids[key] {
				t.Error("Duplicate file ID generated")
			}
			ids[key] = true
		}
	})
}

func TestOpenFileStorage(t *testing.T) {
	t.Run("StoreAndRetrieve", func(t *testing.T) {
		h := NewHandler()

		fileID := h.GenerateFileID()
		file := &OpenFile{
			FileID:      fileID,
			TreeID:      1,
			SessionID:   100,
			Path:        "/test/file.txt",
			ShareName:   "export",
			IsDirectory: false,
		}

		h.StoreOpenFile(file)

		retrieved, ok := h.GetOpenFile(fileID)
		if !ok {
			t.Fatal("OpenFile not found")
		}

		if retrieved.Path != file.Path {
			t.Errorf("Path mismatch")
		}
	})

	t.Run("DeleteOpenFile", func(t *testing.T) {
		h := NewHandler()

		fileID := h.GenerateFileID()
		file := &OpenFile{FileID: fileID}
		h.StoreOpenFile(file)

		h.DeleteOpenFile(fileID)

		_, ok := h.GetOpenFile(fileID)
		if ok {
			t.Error("OpenFile should be deleted")
		}
	})
}

// =============================================================================
// Concurrent Access Tests
// =============================================================================

func TestConcurrentSessionCreation(t *testing.T) {
	h := NewHandler()

	var wg sync.WaitGroup
	sessions := make(chan uint64, 100)

	// Create 100 sessions concurrently
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := h.GenerateSessionID()
			sessions <- id
		}()
	}

	wg.Wait()
	close(sessions)

	// Verify all IDs are unique
	ids := make(map[uint64]bool)
	for id := range sessions {
		if ids[id] {
			t.Errorf("Duplicate session ID: %d", id)
		}
		ids[id] = true
	}

	if len(ids) != 100 {
		t.Errorf("Expected 100 unique IDs, got %d", len(ids))
	}
}

func TestConcurrentSessionStorageAndRetrieval(t *testing.T) {
	h := NewHandler()

	var wg sync.WaitGroup

	// Writers: create sessions
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sess := session.NewSession(uint64(id), "127.0.0.1:12345", true, "guest", "")
			h.SessionManager.StoreSession(sess)
		}(i)
	}

	// Readers: read sessions concurrently
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.GetSession(id)
			}
		}(uint64(i))
	}

	wg.Wait()
}

func TestConcurrentPendingAuthOperations(t *testing.T) {
	h := NewHandler()

	var wg sync.WaitGroup

	// Store pending auths
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			pending := &PendingAuth{
				SessionID:  id,
				ClientAddr: "127.0.0.1:12345",
				CreatedAt:  time.Now(),
			}
			h.StorePendingAuth(pending)

			// Simulate auth completion
			time.Sleep(time.Millisecond)
			h.DeletePendingAuth(id)
		}(uint64(i))
	}

	wg.Wait()
}

func TestConcurrentTreeOperations(t *testing.T) {
	h := NewHandler()

	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			treeID := h.GenerateTreeID()
			tree := &TreeConnection{
				TreeID:    treeID,
				SessionID: 1,
				ShareName: "export",
			}
			h.StoreTree(tree)

			// Read back
			_, _ = h.GetTree(treeID)

			// Delete
			h.DeleteTree(treeID)
		}()
	}

	wg.Wait()
}

func TestConcurrentFileOperations(t *testing.T) {
	h := NewHandler()

	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fileID := h.GenerateFileID()
			file := &OpenFile{
				FileID:    fileID,
				TreeID:    1,
				SessionID: 1,
				Path:      "/test/file.txt",
			}
			h.StoreOpenFile(file)

			// Read back
			_, _ = h.GetOpenFile(fileID)

			// Delete
			h.DeleteOpenFile(fileID)
		}()
	}

	wg.Wait()
}

func TestConcurrentMixedOperations(t *testing.T) {
	h := NewHandler()

	var wg sync.WaitGroup
	done := make(chan bool)

	// Continuous session operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				id := h.GenerateSessionID()
				sess := session.NewSession(id, "client", true, "guest", "")
				h.SessionManager.StoreSession(sess)
				h.GetSession(id)
				h.DeleteSession(id)
			}
		}
	}()

	// Continuous pending auth operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				id := h.GenerateSessionID()
				pending := &PendingAuth{SessionID: id}
				h.StorePendingAuth(pending)
				h.GetPendingAuth(id)
				h.DeletePendingAuth(id)
			}
		}
	}()

	// Continuous tree operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				id := h.GenerateTreeID()
				tree := &TreeConnection{TreeID: id}
				h.StoreTree(tree)
				h.GetTree(id)
				h.DeleteTree(id)
			}
		}
	}()

	// Let it run for a bit
	time.Sleep(100 * time.Millisecond)
	close(done)
	wg.Wait()
}

// =============================================================================
// Session Struct Tests
// =============================================================================

func TestSession(t *testing.T) {
	t.Run("NewSessionSetsFields", func(t *testing.T) {
		sess := session.NewSession(1, "127.0.0.1:12345", true, "guest", "DOMAIN")

		if sess.SessionID != 1 {
			t.Errorf("SessionID = %d, want 1", sess.SessionID)
		}
		if !sess.IsGuest {
			t.Error("IsGuest should be true")
		}
		if sess.Username != "guest" {
			t.Errorf("Username = %q, want %q", sess.Username, "guest")
		}
		if sess.Domain != "DOMAIN" {
			t.Errorf("Domain = %q, want %q", sess.Domain, "DOMAIN")
		}
		if sess.ClientAddr != "127.0.0.1:12345" {
			t.Errorf("ClientAddr = %q, want %q", sess.ClientAddr, "127.0.0.1:12345")
		}
	})

	t.Run("CreatedAtIsSet", func(t *testing.T) {
		before := time.Now()
		sess := session.NewSession(1, "client", true, "guest", "")
		after := time.Now()

		if sess.CreatedAt.Before(before) || sess.CreatedAt.After(after) {
			t.Error("CreatedAt should be between before and after creation")
		}
	})
}

// =============================================================================
// PendingAuth Struct Tests
// =============================================================================

func TestPendingAuth(t *testing.T) {
	t.Run("FieldsSet", func(t *testing.T) {
		now := time.Now()
		pending := &PendingAuth{
			SessionID:  123,
			ClientAddr: "192.168.1.1:54321",
			CreatedAt:  now,
		}

		if pending.SessionID != 123 {
			t.Error("SessionID not set correctly")
		}
		if pending.ClientAddr != "192.168.1.1:54321" {
			t.Error("ClientAddr not set correctly")
		}
		if pending.CreatedAt != now {
			t.Error("CreatedAt not set correctly")
		}
	})
}

// =============================================================================
// TreeConnection Struct Tests
// =============================================================================

func TestTreeConnection(t *testing.T) {
	t.Run("FieldsSet", func(t *testing.T) {
		tree := &TreeConnection{
			TreeID:    1,
			SessionID: 100,
			ShareName: "/export",
			ShareType: 1,
			CreatedAt: time.Now(),
		}

		if tree.ShareName != "/export" {
			t.Error("ShareName not set correctly")
		}
	})
}

// =============================================================================
// OpenFile Struct Tests
// =============================================================================

func TestOpenFile(t *testing.T) {
	t.Run("FieldsSet", func(t *testing.T) {
		var fileID [16]byte
		fileID[0] = 0x01

		file := &OpenFile{
			FileID:      fileID,
			TreeID:      1,
			SessionID:   100,
			Path:        "/test/file.txt",
			ShareName:   "export",
			IsDirectory: false,
		}

		if file.Path != "/test/file.txt" {
			t.Error("Path not set correctly")
		}
		if file.IsDirectory {
			t.Error("IsDirectory should be false")
		}
	})

	t.Run("DirectoryFlag", func(t *testing.T) {
		file := &OpenFile{
			IsDirectory: true,
		}

		if !file.IsDirectory {
			t.Error("IsDirectory should be true")
		}
	})
}
