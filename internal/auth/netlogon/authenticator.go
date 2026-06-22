package netlogon

// NetworkLogonRequest encapsulates the parameters for a NETLOGON logon request.
type NetworkLogonRequest struct {
	DomainSID        string
	UserRID          uint32
	GroupRIDs        []uint32
	SessionBaseKey   [16]byte
	SamAccountName   string
	DnsDomainName    string
}

// LogonResult represents the outcome of NETLOGON authentication mapping.
type LogonResult struct {
	UserSID        string
	GroupSIDs      []string
	SessionBaseKey [16]byte
}

// NetlogonAuthenticator defines the interface for NETLOGON passthrough authentication.
type NetlogonAuthenticator interface {
	Logon(req NetworkLogonRequest) (LogonResult, error)
}

// samInfo4ToResult maps NETLOGON parameters to a LogonResult.
// domainSID is the domain SID (e.g., "S-1-5-21-1-2-3").
// userRID is the user's RID (e.g., 1103).
// groupRIDs are the group RIDs (e.g., []uint32{513, 1104}).
// sessionBaseKey is the session key from the NETLOGON response.
// Returns an error if SIDFromRID fails on any RID.
func samInfo4ToResult(domainSID string, userRID uint32, groupRIDs []uint32, sessionBaseKey [16]byte, sAMAccountName, dnsDomainName string) (LogonResult, error) {
	userSID, err := SIDFromRID(domainSID, userRID)
	if err != nil {
		return LogonResult{}, err
	}

	groupSIDs := make([]string, len(groupRIDs))
	for i, rid := range groupRIDs {
		sid, err := SIDFromRID(domainSID, rid)
		if err != nil {
			return LogonResult{}, err
		}
		groupSIDs[i] = sid
	}

	return LogonResult{
		UserSID:        userSID,
		GroupSIDs:      groupSIDs,
		SessionBaseKey: sessionBaseKey,
	}, nil
}
