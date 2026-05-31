package metadata

import (
	"fmt"
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

func TestCookieManager_Bounded(t *testing.T) {
	t.Parallel()

	t.Run("map stays bounded past cap and forward/reverse stay consistent", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()
		dirHandle := FileHandle("export/bigdir")

		// Generate well past the cap to force eviction.
		const n = defaultCookieCap * 3
		for i := 0; i < n; i++ {
			cookie := cm.GenerateCookie(dirHandle, fmt.Sprintf("file-%d.txt", i))
			require.NotEqual(t, uint64(0), cookie)
		}

		cm.mu.Lock()
		idxLen := len(cm.index)
		orderLen := cm.order.Len()
		// Forward map and recency list must agree exactly — eviction
		// removes from both atomically.
		require.Equal(t, idxLen, orderLen, "index and order length diverged")
		for cookie, el := range cm.index {
			entry := el.Value.(*cookieEntry)
			require.Equal(t, cookie, entry.cookie, "index key vs entry cookie mismatch")
		}
		cm.mu.Unlock()

		// Bounded: never exceeds the cap.
		assert.LessOrEqual(t, idxLen, defaultCookieCap, "cookie count exceeded cap")
		// And it actually filled to the cap (eviction kicked in, not a leak the other way).
		assert.Equal(t, defaultCookieCap, idxLen, "expected the LRU to be full at the cap")
	})

	t.Run("recently used cookies survive eviction", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()
		dirHandle := FileHandle("export/hot")

		// A hot cookie generated first, kept alive by repeated GetToken.
		hot := cm.GenerateCookie(dirHandle, "hot.txt")

		for i := 0; i < defaultCookieCap*2; i++ {
			cm.GenerateCookie(dirHandle, fmt.Sprintf("cold-%d.txt", i))
			// Touch the hot cookie so it stays MRU and is never evicted.
			require.Equal(t, "hot.txt", cm.GetToken(hot))
		}

		assert.Equal(t, "hot.txt", cm.GetToken(hot), "actively-used cookie was evicted")
	})

	t.Run("clear empties forward and reverse state", func(t *testing.T) {
		t.Parallel()
		cm := NewCookieManager()
		dirHandle := FileHandle("export/d")

		for i := 0; i < 100; i++ {
			cm.GenerateCookie(dirHandle, fmt.Sprintf("f-%d", i))
		}
		cm.Clear()

		cm.mu.Lock()
		require.Equal(t, 0, len(cm.index))
		require.Equal(t, 0, cm.order.Len())
		cm.mu.Unlock()
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
