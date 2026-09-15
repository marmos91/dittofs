package nfs

// setV3BlockedOps replaces the cached set of NFSv3 procedure names blocked at
// the adapter level. Called from applyNFSSettings on startup and on each
// settings-change event, so the hot RPC dispatch path (isOperationBlocked)
// consults a pre-parsed name-keyed set instead of unmarshalling the stored
// blocklist on every request. A nil/empty slice clears the set.
func (s *NFSAdapter) setV3BlockedOps(opNames []string) {
	var blocked map[string]bool
	if len(opNames) > 0 {
		blocked = make(map[string]bool, len(opNames))
		for _, name := range opNames {
			blocked[name] = true
		}
	}
	s.blockedOpsMu.Lock()
	s.v3BlockedOps = blocked
	s.blockedOpsMu.Unlock()
}

// isOperationBlocked checks if the given NFSv3 procedure is blocked via adapter
// settings. It consults the pre-parsed v3BlockedOps set (populated from the
// SettingsWatcher in applyNFSSettings, with hot-reload support) so the hot
// dispatch path does a single map lookup rather than a per-RPC JSON unmarshal.
// NFSv4 has its own blocked ops mechanism via Handler.SetBlockedOps.
func (c *NFSConnection) isOperationBlocked(opName string) bool {
	c.server.blockedOpsMu.RLock()
	blocked := c.server.v3BlockedOps[opName]
	c.server.blockedOpsMu.RUnlock()
	return blocked
}
