package netlogon

import (
	"fmt"
	"strings"
)

// SIDFromRID appends a RID to a domain SID, returning the full user/group SID.
// domainSID must be a valid SID string (e.g., "S-1-5-21-1004336348-1177238915-682003330").
// rid must be non-negative. Returns an error if domainSID is malformed.
func SIDFromRID(domainSID string, rid uint32) (string, error) {
	if !strings.HasPrefix(domainSID, "S-1-") {
		return "", fmt.Errorf("invalid domain SID: must start with S-1-")
	}

	// Validate that domainSID contains valid sub-authorities
	parts := strings.Split(domainSID, "-")
	if len(parts) < 4 {
		return "", fmt.Errorf("invalid domain SID: too few components")
	}

	// Ensure each part after "S-1" is a valid non-negative integer
	for _, part := range parts[1:] {
		if part == "" {
			return "", fmt.Errorf("invalid domain SID: empty sub-authority")
		}
		// Basic validation: part should be all digits
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("invalid domain SID: non-numeric sub-authority")
			}
		}
	}

	return fmt.Sprintf("%s-%d", domainSID, rid), nil
}
