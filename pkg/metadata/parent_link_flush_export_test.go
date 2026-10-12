package metadata

import "time"

// ForceDirectoryTimeFlushForTest makes the next bump exercise persistence
// without depending on an operation taking longer than the flush interval.
func ForceDirectoryTimeFlushForTest(s *Service, handles ...FileHandle) {
	s.dirTimes.mu.Lock()
	defer s.dirTimes.mu.Unlock()
	for _, handle := range handles {
		key := handleKey(handle)
		if s.dirTimes.pending[key] == nil {
			s.dirTimes.pending[key] = &dirTimes{}
		}
		s.dirTimes.pending[key].lastFlush = time.Time{}
	}
}
