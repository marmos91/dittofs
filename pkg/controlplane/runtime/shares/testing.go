package shares

// InjectShareForTesting inserts a share directly, bypassing AddShare validation.
func (s *Service) InjectShareForTesting(share *Share) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[share.Name] = share
}
