//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestHardLink tests creating hard links to files
func TestHardLink(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create original file with content
		originalPath := tc.Path("original.txt")
		content := []byte("This is the original file content")
		err := os.WriteFile(originalPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create original file: %v", err)
		}

		// Create hard link
		linkPath := tc.Path("hardlink.txt")
		err = os.Link(originalPath, linkPath)
		if err != nil {
			t.Fatalf("Failed to create hard link: %v", err)
		}

		// Note: Inode verification is platform-specific and not critical for this test
		// The key test is that modifications through one link are visible through the other

		// Verify both have the same content
		linkContent, err := os.ReadFile(linkPath)
		if err != nil {
			t.Fatalf("Failed to read hard link: %v", err)
		}

		if !bytes.Equal(linkContent, content) {
			t.Errorf("Hard link content mismatch: got %q, want %q", linkContent, content)
		}

		// Modify through the hard link
		newContent := []byte("Modified through hard link")
		err = os.WriteFile(linkPath, newContent, 0644)
		if err != nil {
			t.Fatalf("Failed to write to hard link: %v", err)
		}

		// Verify change is visible through original
		originalContent, err := os.ReadFile(originalPath)
		if err != nil {
			t.Fatalf("Failed to read original file: %v", err)
		}

		if !bytes.Equal(originalContent, newContent) {
			t.Errorf("Original file not updated: got %q, want %q", originalContent, newContent)
		}

		// Remove original file
		err = os.Remove(originalPath)
		if err != nil {
			t.Fatalf("Failed to remove original file: %v", err)
		}

		// Verify hard link still exists with content
		linkContent, err = os.ReadFile(linkPath)
		if err != nil {
			t.Fatalf("Failed to read hard link after original deleted: %v", err)
		}

		if !bytes.Equal(linkContent, newContent) {
			t.Errorf("Hard link content changed after original deleted: got %q, want %q", linkContent, newContent)
		}
	})
}

// TestHardLinkMultiple tests creating multiple hard links to the same file
func TestHardLinkMultiple(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create original file
		originalPath := tc.Path("original.txt")
		content := []byte("Original content")
		err := os.WriteFile(originalPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create original file: %v", err)
		}

		// Create multiple hard links
		link1Path := tc.Path("link1.txt")
		link2Path := tc.Path("link2.txt")
		link3Path := tc.Path("link3.txt")

		err = os.Link(originalPath, link1Path)
		if err != nil {
			t.Fatalf("Failed to create link1: %v", err)
		}

		err = os.Link(originalPath, link2Path)
		if err != nil {
			t.Fatalf("Failed to create link2: %v", err)
		}

		err = os.Link(originalPath, link3Path)
		if err != nil {
			t.Fatalf("Failed to create link3: %v", err)
		}

		// Modify through one link
		newContent := []byte("Modified content")
		err = os.WriteFile(link2Path, newContent, 0644)
		if err != nil {
			t.Fatalf("Failed to write to link2: %v", err)
		}

		// Verify all links see the change
		for _, path := range []string{originalPath, link1Path, link2Path, link3Path} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", filepath.Base(path), err)
			}
			if !bytes.Equal(data, newContent) {
				t.Errorf("%s has wrong content: got %q, want %q", filepath.Base(path), data, newContent)
			}
		}

		// Remove all but one link
		if err := os.Remove(originalPath); err != nil {
			t.Fatalf("Failed to remove original: %v", err)
		}
		if err := os.Remove(link1Path); err != nil {
			t.Fatalf("Failed to remove link1: %v", err)
		}
		if err := os.Remove(link2Path); err != nil {
			t.Fatalf("Failed to remove link2: %v", err)
		}

		// Verify last link still has content
		data, err := os.ReadFile(link3Path)
		if err != nil {
			t.Fatalf("Failed to read last link: %v", err)
		}
		if !bytes.Equal(data, newContent) {
			t.Errorf("Last link has wrong content: got %q, want %q", data, newContent)
		}
	})
}
