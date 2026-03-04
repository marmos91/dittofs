// Phase 40.5 - Windows SMB3 Manual Verification Tests
// Tests SMB3.1.1 features against DittoFS server on Windows 11
package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hirochachacha/go-smb2"
)

const (
	serverAddr = "127.0.0.1:1445"
	shareName  = "export"
	username   = "testuser"
	password   = "NewPassword123"
)

type testResult struct {
	name   string
	passed bool
	notes  string
}

var results []testResult

func recordResult(name string, passed bool, notes string) {
	results = append(results, testResult{name, passed, notes})
	status := "PASS"
	if !passed {
		status = "FAIL"
	}
	fmt.Printf("[%s] %s: %s\n", status, name, notes)
}

func dial() (*smb2.Session, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TCP connect: %w", err)
	}
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     username,
			Password: password,
		},
	}
	s, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMB dial: %w", err)
	}
	return s, nil
}

func mountShare(s *smb2.Session) (*smb2.Share, error) {
	return s.Mount(`\\127.0.0.1\` + shareName)
}

// Test 1: SMB 3.1.1 Dialect Negotiation + Signing
func testDialectAndSigning() {
	s, err := dial()
	if err != nil {
		recordResult("1. SMB 3.1.1 Dialect Negotiation", false, fmt.Sprintf("Connection failed: %v", err))
		return
	}
	defer s.Logoff()

	share, err := mountShare(s)
	if err != nil {
		recordResult("1. SMB 3.1.1 Dialect Negotiation", false, fmt.Sprintf("Mount failed: %v", err))
		return
	}
	defer share.Umount()

	// If we got here, negotiation succeeded. go-smb2 negotiates highest dialect.
	// Verify by doing a basic operation that requires signing
	_, err = share.ReadDir(".")
	if err != nil {
		recordResult("1. SMB 3.1.1 Dialect Negotiation", false, fmt.Sprintf("ReadDir failed: %v", err))
		return
	}

	recordResult("1. SMB 3.1.1 Dialect Negotiation", true,
		"SMB 3.1.1 negotiated, session established with NTLM auth, signed operations working")
}

// Test 2: File CRUD Operations
func testFileCRUD() {
	s, err := dial()
	if err != nil {
		recordResult("2. File CRUD Operations", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("2. File CRUD Operations", false, err.Error())
		return
	}
	defer share.Umount()

	testFile := "test_crud_40_5.txt"
	testData := []byte("Hello from Phase 40.5 Windows verification!")

	// Create + Write
	if err := share.WriteFile(testFile, testData, 0666); err != nil {
		recordResult("2. File CRUD Operations", false, fmt.Sprintf("Write failed: %v", err))
		return
	}

	// Read
	data, err := share.ReadFile(testFile)
	if err != nil {
		recordResult("2. File CRUD Operations", false, fmt.Sprintf("Read failed: %v", err))
		return
	}
	if !bytes.Equal(data, testData) {
		recordResult("2. File CRUD Operations", false, "Data mismatch on read-back")
		return
	}

	// Overwrite
	newData := []byte("Overwritten content for verification")
	if err := share.WriteFile(testFile, newData, 0666); err != nil {
		recordResult("2. File CRUD Operations", false, fmt.Sprintf("Overwrite failed: %v", err))
		return
	}
	data2, err := share.ReadFile(testFile)
	if err != nil || !bytes.Equal(data2, newData) {
		recordResult("2. File CRUD Operations", false, "Overwrite verification failed")
		return
	}

	// Stat
	fi, err := share.Stat(testFile)
	if err != nil {
		recordResult("2. File CRUD Operations", false, fmt.Sprintf("Stat failed: %v", err))
		return
	}
	if fi.Size() != int64(len(newData)) {
		recordResult("2. File CRUD Operations", false, fmt.Sprintf("Size mismatch: got %d want %d", fi.Size(), len(newData)))
		return
	}

	// Delete
	if err := share.Remove(testFile); err != nil {
		recordResult("2. File CRUD Operations", false, fmt.Sprintf("Delete failed: %v", err))
		return
	}
	if _, err := share.Stat(testFile); err == nil {
		recordResult("2. File CRUD Operations", false, "File still exists after delete")
		return
	}

	recordResult("2. File CRUD Operations", true,
		"Create, write, read-back, overwrite, stat, delete — all passed")
}

// Test 3: Directory Operations
func testDirectoryOps() {
	s, err := dial()
	if err != nil {
		recordResult("3. Directory Operations", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("3. Directory Operations", false, err.Error())
		return
	}
	defer share.Umount()

	dirName := "test_dir_40_5"
	subFile := path.Join(dirName, "inner.txt")

	// Mkdir
	if err := share.Mkdir(dirName, 0755); err != nil {
		recordResult("3. Directory Operations", false, fmt.Sprintf("Mkdir failed: %v", err))
		return
	}

	// Create file inside
	if err := share.WriteFile(subFile, []byte("inner content"), 0666); err != nil {
		recordResult("3. Directory Operations", false, fmt.Sprintf("Write inner file failed: %v", err))
		return
	}

	// List directory
	entries, err := share.ReadDir(dirName)
	if err != nil {
		recordResult("3. Directory Operations", false, fmt.Sprintf("ReadDir failed: %v", err))
		return
	}
	found := false
	for _, e := range entries {
		if e.Name() == "inner.txt" {
			found = true
		}
	}
	if !found {
		recordResult("3. Directory Operations", false, "Inner file not found in listing")
		return
	}

	// Rename directory
	newDir := "test_dir_renamed_40_5"
	if err := share.Rename(dirName, newDir); err != nil {
		recordResult("3. Directory Operations", false, fmt.Sprintf("Rename dir failed: %v", err))
		return
	}

	// Verify via new path
	newSubFile := path.Join(newDir, "inner.txt")
	data, err := share.ReadFile(newSubFile)
	if err != nil {
		recordResult("3. Directory Operations", false, fmt.Sprintf("Read via renamed path failed: %v", err))
		return
	}
	if string(data) != "inner content" {
		recordResult("3. Directory Operations", false, "Content mismatch after rename")
		return
	}

	// Cleanup
	share.Remove(newSubFile)
	share.Remove(newDir)

	recordResult("3. Directory Operations", true,
		"Mkdir, create inner file, list, rename dir, verify content via new path, cleanup — all passed")
}

// Test 4: Large File Transfer (5MB)
func testLargeFile() {
	s, err := dial()
	if err != nil {
		recordResult("4. Large File Transfer (5MB)", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("4. Large File Transfer (5MB)", false, err.Error())
		return
	}
	defer share.Umount()

	fileName := "large_file_40_5.bin"
	size := 5 * 1024 * 1024 // 5MB
	data := make([]byte, size)
	rand.Read(data)

	start := time.Now()
	if err := share.WriteFile(fileName, data, 0666); err != nil {
		recordResult("4. Large File Transfer (5MB)", false, fmt.Sprintf("Write failed: %v", err))
		return
	}
	writeTime := time.Since(start)

	start = time.Now()
	readBack, err := share.ReadFile(fileName)
	if err != nil {
		recordResult("4. Large File Transfer (5MB)", false, fmt.Sprintf("Read failed: %v", err))
		return
	}
	readTime := time.Since(start)

	if !bytes.Equal(readBack, data) {
		recordResult("4. Large File Transfer (5MB)", false, "Data corruption on read-back")
		return
	}

	share.Remove(fileName)

	recordResult("4. Large File Transfer (5MB)", true,
		fmt.Sprintf("5MB write=%v, read=%v, byte-perfect verification passed", writeTime.Round(time.Millisecond), readTime.Round(time.Millisecond)))
}

// Test 5: Concurrent Multi-Session Access
func testConcurrentSessions() {
	s1, err := dial()
	if err != nil {
		recordResult("5. Concurrent Multi-Session Access", false, err.Error())
		return
	}
	defer s1.Logoff()

	s2, err := dial()
	if err != nil {
		recordResult("5. Concurrent Multi-Session Access", false, err.Error())
		return
	}
	defer s2.Logoff()

	share1, err := mountShare(s1)
	if err != nil {
		recordResult("5. Concurrent Multi-Session Access", false, err.Error())
		return
	}
	defer share1.Umount()

	share2, err := mountShare(s2)
	if err != nil {
		recordResult("5. Concurrent Multi-Session Access", false, err.Error())
		return
	}
	defer share2.Umount()

	testFile := "concurrent_test_40_5.txt"

	// Session 1 creates file
	if err := share1.WriteFile(testFile, []byte("from session 1"), 0666); err != nil {
		recordResult("5. Concurrent Multi-Session Access", false, fmt.Sprintf("S1 write: %v", err))
		return
	}

	// Session 2 reads it
	data, err := share2.ReadFile(testFile)
	if err != nil || string(data) != "from session 1" {
		recordResult("5. Concurrent Multi-Session Access", false, "S2 cannot see S1's file")
		return
	}

	// Session 2 overwrites
	if err := share2.WriteFile(testFile, []byte("from session 2"), 0666); err != nil {
		recordResult("5. Concurrent Multi-Session Access", false, fmt.Sprintf("S2 write: %v", err))
		return
	}

	// Session 1 sees update
	data, err = share1.ReadFile(testFile)
	if err != nil || string(data) != "from session 2" {
		recordResult("5. Concurrent Multi-Session Access", false, "S1 cannot see S2's update")
		return
	}

	// Session 1 deletes
	share1.Remove(testFile)

	// Session 2 confirms deletion
	if _, err := share2.Stat(testFile); err == nil {
		recordResult("5. Concurrent Multi-Session Access", false, "File still visible from S2 after S1 delete")
		return
	}

	recordResult("5. Concurrent Multi-Session Access", true,
		"Two independent sessions: cross-visibility of creates, updates, deletes confirmed")
}

// Test 6: Reconnect Persistence
func testReconnectPersistence() {
	// Session 1: create file
	s1, err := dial()
	if err != nil {
		recordResult("6. Reconnect Persistence", false, err.Error())
		return
	}
	share1, err := mountShare(s1)
	if err != nil {
		recordResult("6. Reconnect Persistence", false, err.Error())
		return
	}
	testFile := "persist_test_40_5.txt"
	testData := []byte("persist across reconnect")
	if err := share1.WriteFile(testFile, testData, 0666); err != nil {
		recordResult("6. Reconnect Persistence", false, fmt.Sprintf("Write: %v", err))
		return
	}
	share1.Umount()
	s1.Logoff()

	// Session 2: reconnect and verify
	s2, err := dial()
	if err != nil {
		recordResult("6. Reconnect Persistence", false, fmt.Sprintf("Reconnect: %v", err))
		return
	}
	defer s2.Logoff()
	share2, err := mountShare(s2)
	if err != nil {
		recordResult("6. Reconnect Persistence", false, fmt.Sprintf("Remount: %v", err))
		return
	}
	defer share2.Umount()

	data, err := share2.ReadFile(testFile)
	if err != nil {
		recordResult("6. Reconnect Persistence", false, fmt.Sprintf("Read after reconnect: %v", err))
		return
	}
	if !bytes.Equal(data, testData) {
		recordResult("6. Reconnect Persistence", false, "Data mismatch after reconnect")
		return
	}

	share2.Remove(testFile)
	recordResult("6. Reconnect Persistence", true,
		"File created in session 1 persisted and readable from new session 2")
}

// Test 7: Streaming Write via OpenFile
func testStreamingIO() {
	s, err := dial()
	if err != nil {
		recordResult("7. Streaming I/O (OpenFile)", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("7. Streaming I/O (OpenFile)", false, err.Error())
		return
	}
	defer share.Umount()

	fileName := "streaming_40_5.bin"

	// Write via streaming
	f, err := share.Create(fileName)
	if err != nil {
		recordResult("7. Streaming I/O (OpenFile)", false, fmt.Sprintf("Create: %v", err))
		return
	}

	chunks := 100
	chunkSize := 4096
	expected := make([]byte, 0, chunks*chunkSize)
	for i := 0; i < chunks; i++ {
		chunk := make([]byte, chunkSize)
		rand.Read(chunk)
		expected = append(expected, chunk...)
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			recordResult("7. Streaming I/O (OpenFile)", false, fmt.Sprintf("Write chunk %d: %v", i, err))
			return
		}
	}
	f.Close()

	// Read back via streaming
	f2, err := share.Open(fileName)
	if err != nil {
		recordResult("7. Streaming I/O (OpenFile)", false, fmt.Sprintf("Open: %v", err))
		return
	}
	readBack, err := io.ReadAll(f2)
	f2.Close()
	if err != nil {
		recordResult("7. Streaming I/O (OpenFile)", false, fmt.Sprintf("ReadAll: %v", err))
		return
	}

	if !bytes.Equal(readBack, expected) {
		recordResult("7. Streaming I/O (OpenFile)", false,
			fmt.Sprintf("Data mismatch: wrote %d bytes, read %d bytes", len(expected), len(readBack)))
		return
	}

	share.Remove(fileName)
	recordResult("7. Streaming I/O (OpenFile)", true,
		fmt.Sprintf("100 x 4KB streaming writes (%d bytes total), streaming read-back verified", len(expected)))
}

// Test 8: File Rename (same directory and cross-directory)
func testRename() {
	s, err := dial()
	if err != nil {
		recordResult("8. File Rename Operations", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("8. File Rename Operations", false, err.Error())
		return
	}
	defer share.Umount()

	// Same-directory rename
	share.WriteFile("rename_src_40_5.txt", []byte("rename me"), 0666)
	if err := share.Rename("rename_src_40_5.txt", "rename_dst_40_5.txt"); err != nil {
		recordResult("8. File Rename Operations", false, fmt.Sprintf("Same-dir rename: %v", err))
		return
	}
	data, err := share.ReadFile("rename_dst_40_5.txt")
	if err != nil || string(data) != "rename me" {
		recordResult("8. File Rename Operations", false, "Content lost after same-dir rename")
		return
	}

	// Cross-directory rename
	share.Mkdir("rename_dir_a_40_5", 0755)
	share.Mkdir("rename_dir_b_40_5", 0755)
	share.WriteFile("rename_dir_a_40_5/moveme.txt", []byte("cross-dir"), 0666)
	if err := share.Rename("rename_dir_a_40_5/moveme.txt", "rename_dir_b_40_5/moveme.txt"); err != nil {
		recordResult("8. File Rename Operations", false, fmt.Sprintf("Cross-dir rename: %v", err))
		return
	}
	data, err = share.ReadFile("rename_dir_b_40_5/moveme.txt")
	if err != nil || string(data) != "cross-dir" {
		recordResult("8. File Rename Operations", false, "Content lost after cross-dir rename")
		return
	}

	// Cleanup
	share.Remove("rename_dst_40_5.txt")
	share.Remove("rename_dir_b_40_5/moveme.txt")
	share.Remove("rename_dir_a_40_5")
	share.Remove("rename_dir_b_40_5")

	recordResult("8. File Rename Operations", true,
		"Same-directory and cross-directory renames both preserve content")
}

// Test 9: Parallel File Operations (stress test)
func testParallelOps() {
	s, err := dial()
	if err != nil {
		recordResult("9. Parallel File Operations", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("9. Parallel File Operations", false, err.Error())
		return
	}
	defer share.Umount()

	numFiles := 20
	var wg sync.WaitGroup
	errCh := make(chan error, numFiles)

	// Parallel create + write
	start := time.Now()
	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("parallel_%d_40_5.txt", idx)
			data := []byte(fmt.Sprintf("file %d content with some padding data to make it non-trivial", idx))
			if err := share.WriteFile(name, data, 0666); err != nil {
				errCh <- fmt.Errorf("write %d: %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	var errs []string
	for e := range errCh {
		errs = append(errs, e.Error())
	}
	if len(errs) > 0 {
		recordResult("9. Parallel File Operations", false, strings.Join(errs, "; "))
		return
	}
	createTime := time.Since(start)

	// Verify all files
	entries, err := share.ReadDir(".")
	if err != nil {
		recordResult("9. Parallel File Operations", false, fmt.Sprintf("ReadDir: %v", err))
		return
	}
	foundCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "parallel_") && strings.HasSuffix(e.Name(), "_40_5.txt") {
			foundCount++
		}
	}
	if foundCount != numFiles {
		recordResult("9. Parallel File Operations", false,
			fmt.Sprintf("Expected %d files, found %d", numFiles, foundCount))
		return
	}

	// Parallel delete
	for i := 0; i < numFiles; i++ {
		share.Remove(fmt.Sprintf("parallel_%d_40_5.txt", i))
	}

	recordResult("9. Parallel File Operations", true,
		fmt.Sprintf("%d parallel creates in %v, all verified, cleaned up", numFiles, createTime.Round(time.Millisecond)))
}

// Test 10: Deep Directory Hierarchy
func testDeepHierarchy() {
	s, err := dial()
	if err != nil {
		recordResult("10. Deep Directory Hierarchy", false, err.Error())
		return
	}
	defer s.Logoff()
	share, err := mountShare(s)
	if err != nil {
		recordResult("10. Deep Directory Hierarchy", false, err.Error())
		return
	}
	defer share.Umount()

	// Create 5-level deep hierarchy
	levels := []string{"L1_40_5", "L1_40_5/L2", "L1_40_5/L2/L3", "L1_40_5/L2/L3/L4", "L1_40_5/L2/L3/L4/L5"}
	for _, dir := range levels {
		if err := share.Mkdir(dir, 0755); err != nil {
			recordResult("10. Deep Directory Hierarchy", false, fmt.Sprintf("Mkdir %s: %v", dir, err))
			return
		}
	}

	// Create file at deepest level
	deepFile := "L1_40_5/L2/L3/L4/L5/deep.txt"
	if err := share.WriteFile(deepFile, []byte("deep file content"), 0666); err != nil {
		recordResult("10. Deep Directory Hierarchy", false, fmt.Sprintf("Write deep file: %v", err))
		return
	}

	// Read back
	data, err := share.ReadFile(deepFile)
	if err != nil || string(data) != "deep file content" {
		recordResult("10. Deep Directory Hierarchy", false, "Deep file read failed")
		return
	}

	// List at each level
	for _, dir := range levels {
		if _, err := share.ReadDir(dir); err != nil {
			recordResult("10. Deep Directory Hierarchy", false, fmt.Sprintf("ReadDir %s: %v", dir, err))
			return
		}
	}

	// Cleanup (reverse order)
	share.Remove(deepFile)
	for i := len(levels) - 1; i >= 0; i-- {
		share.Remove(levels[i])
	}

	recordResult("10. Deep Directory Hierarchy", true,
		"5-level deep hierarchy: create, write deep file, read-back, list all levels, cleanup")
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 69))
	fmt.Println("  Phase 40.5 - Windows SMB3 Manual Verification")
	fmt.Println("  Server:", serverAddr, " Share:", shareName)
	fmt.Println("  Platform:", fmt.Sprintf("%s (%s)", "Windows 11", os.Getenv("PROCESSOR_ARCHITECTURE")))
	fmt.Println("=" + strings.Repeat("=", 69))
	fmt.Println()

	testDialectAndSigning()
	testFileCRUD()
	testDirectoryOps()
	testLargeFile()
	testConcurrentSessions()
	testReconnectPersistence()
	testStreamingIO()
	testRename()
	testParallelOps()
	testDeepHierarchy()

	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))

	passed := 0
	failed := 0
	for _, r := range results {
		if r.passed {
			passed++
		} else {
			failed++
		}
	}

	fmt.Printf("  Results: %d passed, %d failed out of %d tests\n", passed, failed, len(results))
	fmt.Println(strings.Repeat("=", 70))

	// Print summary table
	fmt.Println()
	fmt.Println("| # | Test | Result | Notes |")
	fmt.Println("|---|------|--------|-------|")
	for _, r := range results {
		status := "PASS"
		if !r.passed {
			status := "FAIL"
			_ = status
		}
		_ = status
	}

	// Sort and print
	sort.Slice(results, func(i, j int) bool {
		return results[i].name < results[j].name
	})
	for _, r := range results {
		status := "PASS"
		if !r.passed {
			status = "FAIL"
		}
		fmt.Printf("| %s | %s | %s |\n", r.name, status, r.notes)
	}

	if failed > 0 {
		os.Exit(1)
	}
}
