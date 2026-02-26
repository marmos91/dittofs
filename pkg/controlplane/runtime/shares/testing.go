package shares

// InjectShareForTesting directly inserts a share into the registry.
// This is intended ONLY for unit tests that need to set up share state
// without going through the full AddShare flow.
func (s *Service) InjectShareForTesting(share *Share) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[share.Name] = share
}
