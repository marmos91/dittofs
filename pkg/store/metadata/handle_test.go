package metadata

import (
	"testing"
)

func TestEncodeShareHandle(t *testing.T) {
	tests := []struct {
		name      string
		shareName string
		path      string
		expected  string
	}{
		{
			name:      "simple path",
			shareName: "/export",
			path:      "/file.txt",
			expected:  "/export:/file.txt",
		},
		{
			name:      "nested path",
			shareName: "/export",
			path:      "/documents/report.pdf",
			expected:  "/export:/documents/report.pdf",
		},
		{
			name:      "share without leading slash",
			shareName: "documents",
			path:      "/file.txt",
			expected:  "documents:/file.txt",
		},
		{
			name:      "root path",
			shareName: "/export",
			path:      "/",
			expected:  "/export:/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handle := EncodeShareHandle(tt.shareName, tt.path)
			if string(handle) != tt.expected {
				t.Errorf("EncodeShareHandle() = %q, want %q", string(handle), tt.expected)
			}
		})
	}
}

func TestDecodeShareHandle(t *testing.T) {
	tests := []struct {
		name          string
		handle        FileHandle
		wantShareName string
		wantPath      string
		wantErr       bool
	}{
		{
			name:          "simple path",
			handle:        FileHandle("/export:/file.txt"),
			wantShareName: "/export",
			wantPath:      "/file.txt",
			wantErr:       false,
		},
		{
			name:          "nested path",
			handle:        FileHandle("/export:/documents/report.pdf"),
			wantShareName: "/export",
			wantPath:      "/documents/report.pdf",
			wantErr:       false,
		},
		{
			name:          "share without leading slash",
			handle:        FileHandle("documents:/file.txt"),
			wantShareName: "documents",
			wantPath:      "/file.txt",
			wantErr:       false,
		},
		{
			name:          "root path",
			handle:        FileHandle("/export:/"),
			wantShareName: "/export",
			wantPath:      "/",
			wantErr:       false,
		},
		{
			name:          "path with colon",
			handle:        FileHandle("/export:/path:with:colons.txt"),
			wantShareName: "/export",
			wantPath:      "/path:with:colons.txt",
			wantErr:       false,
		},
		{
			name:    "missing separator",
			handle:  FileHandle("/export/file.txt"),
			wantErr: true,
		},
		{
			name:    "empty share name",
			handle:  FileHandle(":/file.txt"),
			wantErr: true,
		},
		{
			name:    "empty path",
			handle:  FileHandle("/export:"),
			wantErr: true,
		},
		{
			name:    "path without leading slash",
			handle:  FileHandle("/export:file.txt"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shareName, path, err := DecodeShareHandle(tt.handle)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeShareHandle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if shareName != tt.wantShareName {
					t.Errorf("DecodeShareHandle() shareName = %q, want %q", shareName, tt.wantShareName)
				}
				if path != tt.wantPath {
					t.Errorf("DecodeShareHandle() path = %q, want %q", path, tt.wantPath)
				}
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		shareName string
		path      string
	}{
		{"/export", "/file.txt"},
		{"/export", "/documents/report.pdf"},
		{"documents", "/data.json"},
		{"/export", "/"},
		{"/export", "/path/with:colons.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.shareName+tt.path, func(t *testing.T) {
			// Encode
			handle := EncodeShareHandle(tt.shareName, tt.path)

			// Decode
			shareName, path, err := DecodeShareHandle(handle)
			if err != nil {
				t.Fatalf("DecodeShareHandle() error = %v", err)
			}

			// Verify round trip
			if shareName != tt.shareName {
				t.Errorf("Round trip shareName = %q, want %q", shareName, tt.shareName)
			}
			if path != tt.path {
				t.Errorf("Round trip path = %q, want %q", path, tt.path)
			}
		})
	}
}

func TestHandleToINode(t *testing.T) {
	tests := []struct {
		name   string
		handle FileHandle
	}{
		{"simple path", FileHandle("/export:/file.txt")},
		{"nested path", FileHandle("/export:/documents/report.pdf")},
		{"root path", FileHandle("/export:/")},
	}

	// Test that same handle always produces same ID
	for _, tt := range tests {
		t.Run(tt.name+" consistency", func(t *testing.T) {
			id1 := HandleToINode(tt.handle)
			id2 := HandleToINode(tt.handle)
			if id1 != id2 {
				t.Errorf("HandleToINode() not consistent: %d != %d", id1, id2)
			}
		})
	}

	// Test that different handles produce different IDs (with high probability)
	t.Run("uniqueness", func(t *testing.T) {
		ids := make(map[uint64]string)
		for _, tt := range tests {
			id := HandleToINode(tt.handle)
			if existingHandle, exists := ids[id]; exists {
				t.Errorf("HandleToINode() collision: %q and %q both produce ID %d",
					tt.handle, existingHandle, id)
			}
			ids[id] = string(tt.handle)
		}
	})

	// Test empty handle
	t.Run("empty handle", func(t *testing.T) {
		id := HandleToINode(FileHandle{})
		if id != 0 {
			t.Errorf("HandleToINode(empty) = %d, want 0", id)
		}
	})
}
