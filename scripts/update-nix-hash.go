// Command update-nix-hash automatically updates the vendorHash in flake.nix
// after Go dependencies have changed.
//
// Usage (from the repo root):
//
//	go run scripts/update-nix-hash.go
//
// It is also run automatically by .github/workflows/nix-update-hash.yml
// whenever go.mod or go.sum change.
package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	flakeFile = "flake.nix"
	fakeHash  = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
)

func main() {
	log.SetFlags(0)

	// Check if flake.nix exists
	if _, err := os.Stat(flakeFile); err != nil {
		log.Fatalf("Error: %s not found. Run this from the project root.", flakeFile)
	}

	// Read the current flake.nix
	content, err := os.ReadFile(flakeFile)
	if err != nil {
		log.Fatalf("Error reading %s: %v", flakeFile, err)
	}

	// Find current vendorHash
	hashRe := regexp.MustCompile(`vendorHash = "(sha256-[A-Za-z0-9+/=]+)";`)
	matches := hashRe.FindSubmatch(content)
	if matches == nil {
		log.Fatalf("Error: Could not find vendorHash in %s", flakeFile)
	}
	currentHash := string(matches[1])

	log.Printf("Current vendorHash: %s", currentHash)
	log.Println("Updating vendorHash in flake.nix...")

	// Replace with fake hash to trigger a Nix mismatch error
	updatedContent := hashRe.ReplaceAll(content, fmt.Appendf(nil, `vendorHash = "%s";`, fakeHash))

	// Write temporary flake.nix
	if err := os.WriteFile(flakeFile, updatedContent, 0644); err != nil {
		log.Fatalf("Error writing %s: %v", flakeFile, err)
	}

	// Run nix build to get the correct hash (expected to fail)
	log.Println("Running nix build to determine correct hash...")
	cmd := exec.Command("nix", "build", ".#default")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = os.Stdout
	_ = cmd.Run()

	// Extract the correct hash from stderr
	output := stderr.String()
	correctHash := extractHash(output)

	if correctHash == "" {
		// Restore original content
		if err := os.WriteFile(flakeFile, content, 0644); err != nil {
			log.Printf("Warning: Failed to restore original flake.nix: %v", err)
		}
		log.Fatalf("Error: Could not extract hash from nix build output.\n%s", output)
	}

	log.Printf("Extracted hash: %s", correctHash)

	// If hash hasn't changed, we're done
	if correctHash == currentHash {
		// Restore original
		if err := os.WriteFile(flakeFile, content, 0644); err != nil {
			log.Fatalf("Error restoring %s: %v", flakeFile, err)
		}
		log.Println("✓ vendorHash is already up to date")
		return
	}

	// Update with correct hash (reuse the regex so only the vendorHash line is touched)
	finalContent := hashRe.ReplaceAll(content, fmt.Appendf(nil, `vendorHash = "%s";`, correctHash))
	if err := os.WriteFile(flakeFile, finalContent, 0644); err != nil {
		log.Fatalf("Error writing %s: %v", flakeFile, err)
	}

	log.Printf("Updated vendorHash: %s -> %s", currentHash, correctHash)

	// Verify build works with the new hash
	log.Println("Verifying build with new hash...")
	cmd = exec.Command("nix", "build", ".#default")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Fatalf("Error: Build failed with new hash: %v", err)
	}

	log.Println("✓ Build successful with new hash")
}

// extractHash parses the nix build error output to find the correct hash.
func extractHash(output string) string {
	// Look for "got:    sha256-..."
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "got:") {
			// Extract the hash after "got:"
			parts := strings.Fields(line)
			if len(parts) >= 2 && strings.HasPrefix(parts[1], "sha256-") {
				return parts[1]
			}
		}
	}
	return ""
}
