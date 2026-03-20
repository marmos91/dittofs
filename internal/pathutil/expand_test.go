package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get home dir: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "tilde slash prefix",
			path: "~/foo/bar",
			want: filepath.Join(home, "foo/bar"),
		},
		{
			name: "tilde only",
			path: "~",
			want: home,
		},
		{
			name: "absolute path unchanged",
			path: "/absolute/path",
			want: "/absolute/path",
		},
		{
			name: "relative path unchanged",
			path: "relative/path",
			want: "relative/path",
		},
		{
			name: "empty string unchanged",
			path: "",
			want: "",
		},
		{
			name: "tilde in middle unchanged",
			path: "/foo/~/bar",
			want: "/foo/~/bar",
		},
		{
			name: "tilde without slash unchanged",
			path: "~user/foo",
			want: "~user/foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandPath(tt.path)
			if err != nil {
				t.Fatalf("ExpandPath(%q) returned error: %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
