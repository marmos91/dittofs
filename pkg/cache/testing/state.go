package testing

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RunStateManagementTests tests the state management methods of the Cache interface.
func (suite *CacheTestSuite) RunStateManagementTests(t *testing.T) {
	t.Run("GetStateReturnsNoneForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		state := c.GetState("non-existent")
		if state != cache.StateNone {
			t.Errorf("Expected StateNone for non-existent entry, got %v", state)
		}
	})

	t.Run("NewEntryStartsInBufferingState", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		state := c.GetState(id)
		if state != cache.StateBuffering {
			t.Errorf("Expected StateBuffering for new entry, got %v", state)
		}
	})

	t.Run("SetStateUpdatesState", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Transition to Uploading
		c.SetState(id, cache.StateUploading)
		if state := c.GetState(id); state != cache.StateUploading {
			t.Errorf("Expected StateUploading, got %v", state)
		}

		// Transition to Cached
		c.SetState(id, cache.StateCached)
		if state := c.GetState(id); state != cache.StateCached {
			t.Errorf("Expected StateCached, got %v", state)
		}
	})

	t.Run("SetStateIsNoOpForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		// Should not panic or create entry
		c.SetState("non-existent", cache.StateUploading)

		state := c.GetState("non-existent")
		if state != cache.StateNone {
			t.Errorf("Expected StateNone, got %v", state)
		}
	})

	t.Run("WriteResetsCachedStateToBuffering", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		// Create entry and finalize it
		err := c.Write(ctx, id, []byte("initial"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		c.SetState(id, cache.StateCached)

		// Write new data - should reset to Buffering
		err = c.Write(ctx, id, []byte("updated"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		state := c.GetState(id)
		if state != cache.StateBuffering {
			t.Errorf("Expected StateBuffering after write to cached entry, got %v", state)
		}
	})

	t.Run("WriteAtResetsCachedStateToBuffering", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		// Create entry and finalize it
		err := c.Write(ctx, id, []byte("initial data"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		c.SetState(id, cache.StateCached)

		// WriteAt new data - should reset to Buffering
		err = c.WriteAt(ctx, id, []byte("new"), 0)
		if err != nil {
			t.Fatalf("WriteAt failed: %v", err)
		}

		state := c.GetState(id)
		if state != cache.StateBuffering {
			t.Errorf("Expected StateBuffering after WriteAt to cached entry, got %v", state)
		}
	})

	t.Run("GetFlushedOffsetReturnsZeroForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		offset := c.GetFlushedOffset("non-existent")
		if offset != 0 {
			t.Errorf("Expected 0 for non-existent entry, got %d", offset)
		}
	})

	t.Run("GetFlushedOffsetReturnsZeroForNewEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		offset := c.GetFlushedOffset(id)
		if offset != 0 {
			t.Errorf("Expected 0 for new entry, got %d", offset)
		}
	})

	t.Run("SetFlushedOffsetUpdatesOffset", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello world"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		c.SetFlushedOffset(id, 5)
		if offset := c.GetFlushedOffset(id); offset != 5 {
			t.Errorf("Expected flushed offset 5, got %d", offset)
		}

		c.SetFlushedOffset(id, 11)
		if offset := c.GetFlushedOffset(id); offset != 11 {
			t.Errorf("Expected flushed offset 11, got %d", offset)
		}
	})

	t.Run("SetFlushedOffsetIsNoOpForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		// Should not panic
		c.SetFlushedOffset("non-existent", 100)

		offset := c.GetFlushedOffset("non-existent")
		if offset != 0 {
			t.Errorf("Expected 0, got %d", offset)
		}
	})

	t.Run("WriteResetsFlushedOffset", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		// Create entry with some flushed data
		err := c.Write(ctx, id, []byte("initial"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		c.SetFlushedOffset(id, 7)
		c.SetState(id, cache.StateCached)

		// Write new data - should reset flushed offset
		err = c.Write(ctx, id, []byte("new data"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		offset := c.GetFlushedOffset(id)
		if offset != 0 {
			t.Errorf("Expected flushed offset to reset to 0, got %d", offset)
		}
	})

	t.Run("LastAccessReturnsZeroForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		lastAccess := c.LastAccess("non-existent")
		if !lastAccess.IsZero() {
			t.Errorf("Expected zero time for non-existent entry, got %v", lastAccess)
		}
	})

	t.Run("LastAccessUpdatedOnWrite", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		before := time.Now()
		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		after := time.Now()

		lastAccess := c.LastAccess(id)
		if lastAccess.Before(before) || lastAccess.After(after) {
			t.Errorf("LastAccess %v not in expected range [%v, %v]", lastAccess, before, after)
		}
	})

	t.Run("LastAccessUpdatedOnRead", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Wait a bit to ensure time difference
		time.Sleep(10 * time.Millisecond)

		before := time.Now()
		buf := make([]byte, 5)
		_, err = c.ReadAt(ctx, id, buf, 0)
		if err != nil {
			t.Fatalf("ReadAt failed: %v", err)
		}
		after := time.Now()

		lastAccess := c.LastAccess(id)
		if lastAccess.Before(before) || lastAccess.After(after) {
			t.Errorf("LastAccess %v not in expected range [%v, %v]", lastAccess, before, after)
		}
	})

	t.Run("StateIsDirtyMethod", func(t *testing.T) {
		if !cache.StateBuffering.IsDirty() {
			t.Error("StateBuffering should be dirty")
		}
		if !cache.StateUploading.IsDirty() {
			t.Error("StateUploading should be dirty")
		}
		if cache.StateCached.IsDirty() {
			t.Error("StateCached should not be dirty")
		}
		if cache.StateNone.IsDirty() {
			t.Error("StateNone should not be dirty")
		}
	})

	t.Run("StateStringMethod", func(t *testing.T) {
		tests := []struct {
			state    cache.CacheState
			expected string
		}{
			{cache.StateNone, "None"},
			{cache.StateBuffering, "Buffering"},
			{cache.StateUploading, "Uploading"},
			{cache.StateCached, "Cached"},
			{cache.CacheState(99), "Unknown"},
		}

		for _, tt := range tests {
			if got := tt.state.String(); got != tt.expected {
				t.Errorf("CacheState(%d).String() = %q, want %q", tt.state, got, tt.expected)
			}
		}
	})
}
