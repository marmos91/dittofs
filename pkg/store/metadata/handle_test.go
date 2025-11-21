package metadata

import (
	"testing"

	"github.com/google/uuid"
)

func TestEncodeShareHandle(t *testing.T) {
	tests := []struct {
		name      string
		shareName string
		id        uuid.UUID
		wantErr   bool
	}{
		{
			name:      "simple share with UUID",
			shareName: "/export",
			id:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			wantErr:   false,
		},
		{
			name:      "share without leading slash",
			shareName: "documents",
			id:        uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			wantErr:   false,
		},
		{
			name:      "long share name still under 64 bytes",
			shareName: "/very/long/share/name/path",
			id:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handle, err := EncodeShareHandle(tt.shareName, tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("EncodeShareHandle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				// Verify handle is not empty
				if len(handle) == 0 {
					t.Error("EncodeShareHandle() returned empty handle")
				}
				// Verify handle is under 64 bytes (NFS RFC 1813 limit)
				if len(handle) > 64 {
					t.Errorf("EncodeShareHandle() handle length = %d, want <= 64", len(handle))
				}
				// Verify format is "shareName:uuid"
				expected := tt.shareName + ":" + tt.id.String()
				if string(handle) != expected {
					t.Errorf("EncodeShareHandle() = %q, want %q", string(handle), expected)
				}
			}
		})
	}
}

func TestEncodeFileHandle(t *testing.T) {
	tests := []struct {
		name    string
		file    *File
		wantErr bool
	}{
		{
			name: "regular file",
			file: &File{
				ID:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
				ShareName: "/export",
				Path:      "/documents/report.pdf",
			},
			wantErr: false,
		},
		{
			name: "root directory",
			file: &File{
				ID:        uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
				ShareName: "/export",
				Path:      "/",
			},
			wantErr: false,
		},
		{
			name: "file with empty path",
			file: &File{
				ID:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
				ShareName: "documents",
				Path:      "",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handle, err := EncodeFileHandle(tt.file)
			if (err != nil) != tt.wantErr {
				t.Errorf("EncodeFileHandle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				// Verify handle is not empty
				if len(handle) == 0 {
					t.Error("EncodeFileHandle() returned empty handle")
				}
				// Verify handle is under 64 bytes
				if len(handle) > 64 {
					t.Errorf("EncodeFileHandle() handle length = %d, want <= 64", len(handle))
				}
				// Verify format matches EncodeShareHandle
				expected := tt.file.ShareName + ":" + tt.file.ID.String()
				if string(handle) != expected {
					t.Errorf("EncodeFileHandle() = %q, want %q", string(handle), expected)
				}
			}
		})
	}
}

func TestDecodeFileHandle(t *testing.T) {
	tests := []struct {
		name          string
		handle        FileHandle
		wantShareName string
		wantID        uuid.UUID
		wantErr       bool
	}{
		{
			name:          "valid handle",
			handle:        FileHandle("/export:550e8400-e29b-41d4-a716-446655440000"),
			wantShareName: "/export",
			wantID:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			wantErr:       false,
		},
		{
			name:          "share without leading slash",
			handle:        FileHandle("documents:123e4567-e89b-12d3-a456-426614174000"),
			wantShareName: "documents",
			wantID:        uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			wantErr:       false,
		},
		{
			name:          "long share name",
			handle:        FileHandle("/very/long/share/name:550e8400-e29b-41d4-a716-446655440000"),
			wantShareName: "/very/long/share/name",
			wantID:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			wantErr:       false,
		},
		{
			name:    "missing separator",
			handle:  FileHandle("/export550e8400-e29b-41d4-a716-446655440000"),
			wantErr: true,
		},
		{
			name:    "empty share name",
			handle:  FileHandle(":550e8400-e29b-41d4-a716-446655440000"),
			wantErr: true,
		},
		{
			name:    "invalid UUID",
			handle:  FileHandle("/export:not-a-valid-uuid"),
			wantErr: true,
		},
		{
			name:    "malformed UUID",
			handle:  FileHandle("/export:550e8400-e29b-41d4-a716"),
			wantErr: true,
		},
		{
			name:    "empty handle",
			handle:  FileHandle(""),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shareName, id, err := DecodeFileHandle(tt.handle)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeFileHandle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if shareName != tt.wantShareName {
					t.Errorf("DecodeFileHandle() shareName = %q, want %q", shareName, tt.wantShareName)
				}
				if id != tt.wantID {
					t.Errorf("DecodeFileHandle() id = %v, want %v", id, tt.wantID)
				}
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		shareName string
		id        uuid.UUID
	}{
		{"/export", uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")},
		{"/export", uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")},
		{"documents", uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")},
		{"/data", uuid.MustParse("00000000-0000-0000-0000-000000000000")},
		{"/very/long/share/name", uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")},
	}

	for _, tt := range tests {
		t.Run(tt.shareName+":"+tt.id.String(), func(t *testing.T) {
			// Encode
			handle, err := EncodeShareHandle(tt.shareName, tt.id)
			if err != nil {
				t.Fatalf("EncodeShareHandle() error = %v", err)
			}

			// Decode
			shareName, id, err := DecodeFileHandle(handle)
			if err != nil {
				t.Fatalf("DecodeFileHandle() error = %v", err)
			}

			// Verify round trip
			if shareName != tt.shareName {
				t.Errorf("Round trip shareName = %q, want %q", shareName, tt.shareName)
			}
			if id != tt.id {
				t.Errorf("Round trip id = %v, want %v", id, tt.id)
			}
		})
	}
}

func TestEncodeFileHandleRoundTrip(t *testing.T) {
	tests := []*File{
		{
			ID:        uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			ShareName: "/export",
			Path:      "/documents/report.pdf",
		},
		{
			ID:        uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			ShareName: "data",
			Path:      "/",
		},
		{
			ID:        uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7"),
			ShareName: "/export",
			Path:      "",
		},
	}

	for _, file := range tests {
		t.Run(file.ShareName+":"+file.ID.String(), func(t *testing.T) {
			// Encode
			handle, err := EncodeFileHandle(file)
			if err != nil {
				t.Fatalf("EncodeFileHandle() error = %v", err)
			}

			// Decode
			shareName, id, err := DecodeFileHandle(handle)
			if err != nil {
				t.Fatalf("DecodeFileHandle() error = %v", err)
			}

			// Verify round trip (path is not encoded in handle)
			if shareName != file.ShareName {
				t.Errorf("Round trip shareName = %q, want %q", shareName, file.ShareName)
			}
			if id != file.ID {
				t.Errorf("Round trip id = %v, want %v", id, file.ID)
			}
		})
	}
}

func TestHandleToINode(t *testing.T) {
	// Generate test handles
	id1 := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	id2 := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	id3 := uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")

	handle1, _ := EncodeShareHandle("/export", id1)
	handle2, _ := EncodeShareHandle("/export", id2)
	handle3, _ := EncodeShareHandle("data", id3)

	// Test that same handle always produces same ID
	t.Run("consistency", func(t *testing.T) {
		inode1 := HandleToINode(handle1)
		inode2 := HandleToINode(handle1)
		if inode1 != inode2 {
			t.Errorf("HandleToINode() not consistent: %d != %d", inode1, inode2)
		}
	})

	// Test that different handles produce different IDs
	t.Run("uniqueness", func(t *testing.T) {
		inode1 := HandleToINode(handle1)
		inode2 := HandleToINode(handle2)
		inode3 := HandleToINode(handle3)

		if inode1 == inode2 {
			t.Errorf("HandleToINode() collision: handles %q and %q both produce ID %d",
				handle1, handle2, inode1)
		}
		if inode1 == inode3 {
			t.Errorf("HandleToINode() collision: handles %q and %q both produce ID %d",
				handle1, handle3, inode1)
		}
		if inode2 == inode3 {
			t.Errorf("HandleToINode() collision: handles %q and %q both produce ID %d",
				handle2, handle3, inode2)
		}
	})

	// Test empty handle
	t.Run("empty handle", func(t *testing.T) {
		id := HandleToINode(FileHandle{})
		if id != 0 {
			t.Errorf("HandleToINode(empty) = %d, want 0", id)
		}
	})

	// Test that inode is non-zero for valid handles
	t.Run("non-zero inode", func(t *testing.T) {
		inode := HandleToINode(handle1)
		if inode == 0 {
			t.Error("HandleToINode() returned 0 for valid handle")
		}
	})
}
