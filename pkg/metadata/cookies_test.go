package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCookieManager_GenerateCookie(t *testing.T) {
	t.Parallel()

	t.Run("returns 0 for empty name", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie := cm.GenerateCookie(FileHandle("dir"), "")

		assert.Equal(t, uint64(0), cookie)
	})

	t.Run("returns non-zero for valid name", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie := cm.GenerateCookie(FileHandle("dir"), "file.txt")

		assert.NotEqual(t, uint64(0), cookie)
	})

	t.Run("same inputs produce same cookie", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie1 := cm.GenerateCookie(FileHandle("dir"), "file.txt")
		cookie2 := cm.GenerateCookie(FileHandle("dir"), "file.txt")

		assert.Equal(t, cookie1, cookie2)
	})

	t.Run("different names produce different cookies", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie1 := cm.GenerateCookie(FileHandle("dir"), "file1.txt")
		cookie2 := cm.GenerateCookie(FileHandle("dir"), "file2.txt")

		assert.NotEqual(t, cookie1, cookie2)
	})

	t.Run("different directories produce different cookies", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie1 := cm.GenerateCookie(FileHandle("dir1"), "file.txt")
		cookie2 := cm.GenerateCookie(FileHandle("dir2"), "file.txt")

		assert.NotEqual(t, cookie1, cookie2)
	})
}

func TestCookieManager_GetToken(t *testing.T) {
	t.Parallel()

	t.Run("returns empty for cookie 0", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		token := cm.GetToken(0)

		assert.Empty(t, token)
	})

	t.Run("returns empty for unknown cookie", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		token := cm.GetToken(12345)

		assert.Empty(t, token)
	})

	t.Run("returns token for known cookie", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie := cm.GenerateCookie(FileHandle("dir"), "file.txt")
		token := cm.GetToken(cookie)

		assert.Equal(t, "file.txt", token)
	})

	t.Run("handles multiple entries", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie1 := cm.GenerateCookie(FileHandle("dir"), "file1.txt")
		cookie2 := cm.GenerateCookie(FileHandle("dir"), "file2.txt")
		cookie3 := cm.GenerateCookie(FileHandle("dir"), "file3.txt")

		assert.Equal(t, "file1.txt", cm.GetToken(cookie1))
		assert.Equal(t, "file2.txt", cm.GetToken(cookie2))
		assert.Equal(t, "file3.txt", cm.GetToken(cookie3))
	})
}

func TestCookieManager_Clear(t *testing.T) {
	t.Parallel()

	t.Run("clears all mappings", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		cookie1 := cm.GenerateCookie(FileHandle("dir"), "file1.txt")
		cookie2 := cm.GenerateCookie(FileHandle("dir"), "file2.txt")

		require.NotEmpty(t, cm.GetToken(cookie1))
		require.NotEmpty(t, cm.GetToken(cookie2))

		cm.Clear()

		assert.Empty(t, cm.GetToken(cookie1))
		assert.Empty(t, cm.GetToken(cookie2))
	})
}

func TestCookieManager_Concurrent(t *testing.T) {
	t.Parallel()

	t.Run("handles concurrent access", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()

		done := make(chan bool)
		for i := 0; i < 100; i++ {
			go func(idx int) {
				name := string(rune('a' + idx%26))
				cookie := cm.GenerateCookie(FileHandle("dir"), name)
				_ = cm.GetToken(cookie)
				done <- true
			}(i)
		}

		for i := 0; i < 100; i++ {
			<-done
		}
	})
}

func TestCookieManager_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("full pagination round trip", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()
		dirHandle := FileHandle("export/mydir")

		// Simulate generating cookies for directory entries
		entries := []string{"aaa.txt", "bbb.txt", "ccc.txt", "ddd.txt", "eee.txt"}
		cookies := make([]uint64, len(entries))

		for i, name := range entries {
			cookies[i] = cm.GenerateCookie(dirHandle, name)
		}

		// Simulate resuming from each cookie
		for i, cookie := range cookies {
			token := cm.GetToken(cookie)
			assert.Equal(t, entries[i], token, "cookie %d should resolve to %s", cookie, entries[i])
		}

		// Verify cookies are unique
		seen := make(map[uint64]bool)
		for _, cookie := range cookies {
			assert.False(t, seen[cookie], "duplicate cookie: %d", cookie)
			seen[cookie] = true
		}
	})
}
