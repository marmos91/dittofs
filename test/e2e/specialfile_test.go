//go:build e2e

package e2e

import (
	"os"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCreateFIFO tests creating named pipes (FIFOs) via NFS
func TestCreateFIFO(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		fifoPath := tc.Path("testpipe")

		// Create FIFO (named pipe)
		err := syscall.Mkfifo(fifoPath, 0644)
		if err != nil {
			t.Fatalf("Failed to create FIFO: %v", err)
		}

		// Verify FIFO exists and has correct type
		info, err := os.Lstat(fifoPath)
		if err != nil {
			t.Fatalf("Failed to stat FIFO: %v", err)
		}

		// Check it's a named pipe
		if info.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("Expected named pipe, got mode %v", info.Mode())
		}

		// Verify size is 0
		if info.Size() != 0 {
			t.Errorf("Expected FIFO size 0, got %d", info.Size())
		}

		// Clean up
		err = os.Remove(fifoPath)
		if err != nil {
			t.Fatalf("Failed to remove FIFO: %v", err)
		}
	})
}

// TestCreateSocket tests creating Unix domain sockets via NFS
// Note: This test creates a socket file entry, not a listening socket
func TestCreateSocket(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		sockPath := tc.Path("testsock")

		// Create Unix socket using mknod with S_IFSOCK
		// Note: This creates the socket file entry, not a listening socket
		err := syscall.Mknod(sockPath, syscall.S_IFSOCK|0755, 0)
		if err != nil {
			// Socket creation via mknod may not be supported on all systems
			t.Skipf("Socket creation via mknod not supported: %v", err)
		}

		// Verify socket exists and has correct type
		info, err := os.Lstat(sockPath)
		if err != nil {
			t.Fatalf("Failed to stat socket: %v", err)
		}

		// Check it's a socket
		if info.Mode()&os.ModeSocket == 0 {
			t.Errorf("Expected socket, got mode %v", info.Mode())
		}

		// Clean up
		err = os.Remove(sockPath)
		if err != nil {
			t.Fatalf("Failed to remove socket: %v", err)
		}
	})
}

// mkdev creates a device number from major and minor numbers
// This is cross-platform (works on both Linux and macOS)
func mkdev(major, minor uint32) int {
	return int(unix.Mkdev(major, minor))
}

// TestCreateCharDevice tests creating character device nodes via NFS
// Note: This requires root privileges
func TestCreateCharDevice(t *testing.T) {
	// Skip if not root
	if os.Getuid() != 0 {
		t.Skip("Skipping device creation test - requires root")
	}

	// Skip on macOS - device creation via NFS is not supported
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping device creation test on macOS - not supported via NFS")
	}

	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		devPath := tc.Path("testnull")

		// Create character device like /dev/null (major=1, minor=3)
		dev := mkdev(1, 3)
		err := syscall.Mknod(devPath, syscall.S_IFCHR|0666, dev)
		if err != nil {
			t.Fatalf("Failed to create char device: %v", err)
		}

		// Verify device exists and has correct type
		info, err := os.Lstat(devPath)
		if err != nil {
			t.Fatalf("Failed to stat device: %v", err)
		}

		// Check it's a character device
		if info.Mode()&os.ModeCharDevice == 0 {
			t.Errorf("Expected char device, got mode %v", info.Mode())
		}

		// Clean up
		err = os.Remove(devPath)
		if err != nil {
			t.Fatalf("Failed to remove device: %v", err)
		}
	})
}

// TestCreateBlockDevice tests creating block device nodes via NFS
// Note: This requires root privileges
func TestCreateBlockDevice(t *testing.T) {
	// Skip if not root
	if os.Getuid() != 0 {
		t.Skip("Skipping device creation test - requires root")
	}

	// Skip on macOS - device creation via NFS is not supported
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping device creation test on macOS - not supported via NFS")
	}

	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		devPath := tc.Path("testloop")

		// Create block device like /dev/loop0 (major=7, minor=0)
		dev := mkdev(7, 0)
		err := syscall.Mknod(devPath, syscall.S_IFBLK|0660, dev)
		if err != nil {
			t.Fatalf("Failed to create block device: %v", err)
		}

		// Verify device exists and has correct type
		info, err := os.Lstat(devPath)
		if err != nil {
			t.Fatalf("Failed to stat device: %v", err)
		}

		// Check it's a block device
		if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
			// This is a block device (ModeDevice set but not ModeCharDevice)
		} else {
			t.Errorf("Expected block device, got mode %v", info.Mode())
		}

		// Clean up
		err = os.Remove(devPath)
		if err != nil {
			t.Fatalf("Failed to remove device: %v", err)
		}
	})
}

// TestFIFOInSubdirectory tests creating FIFO in a subdirectory
func TestFIFOInSubdirectory(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create subdirectory
		subdir := tc.Path("subdir")
		err := os.Mkdir(subdir, 0755)
		if err != nil {
			t.Fatalf("Failed to create subdirectory: %v", err)
		}

		fifoPath := tc.Path("subdir/mypipe")

		// Create FIFO in subdirectory
		err = syscall.Mkfifo(fifoPath, 0644)
		if err != nil {
			t.Fatalf("Failed to create FIFO in subdirectory: %v", err)
		}

		// Verify FIFO exists
		info, err := os.Lstat(fifoPath)
		if err != nil {
			t.Fatalf("Failed to stat FIFO: %v", err)
		}

		if info.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("Expected named pipe, got mode %v", info.Mode())
		}

		// Clean up
		os.Remove(fifoPath)
		os.Remove(subdir)
	})
}
