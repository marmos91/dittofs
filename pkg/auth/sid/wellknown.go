package sid

// Well-known SID constants for common Windows security principals.
// These are used across both SMB and NFS adapters for consistent
// identity mapping.

var (
	// WellKnownEveryone is the "Everyone" (World) SID: S-1-1-0.
	// Maps to NFSv4 EVERYONE@ special identifier.
	WellKnownEveryone = ParseSIDMust("S-1-1-0")

	// WellKnownCreatorOwner is the CREATOR OWNER SID: S-1-3-0.
	// Maps to NFSv4 OWNER@ special identifier.
	WellKnownCreatorOwner = ParseSIDMust("S-1-3-0")

	// WellKnownCreatorGroup is the CREATOR GROUP SID: S-1-3-1.
	// Maps to NFSv4 GROUP@ special identifier.
	WellKnownCreatorGroup = ParseSIDMust("S-1-3-1")

	// WellKnownAnonymous is the NT AUTHORITY\ANONYMOUS LOGON SID: S-1-5-7.
	// Used for anonymous/guest connections.
	WellKnownAnonymous = ParseSIDMust("S-1-5-7")

	// WellKnownAuthenticatedUsers is the NT AUTHORITY\Authenticated Users SID: S-1-5-11.
	WellKnownAuthenticatedUsers = ParseSIDMust("S-1-5-11")

	// WellKnownSystem is the NT AUTHORITY\SYSTEM SID: S-1-5-18.
	WellKnownSystem = ParseSIDMust("S-1-5-18")

	// WellKnownAdministrators is the BUILTIN\Administrators SID: S-1-5-32-544.
	WellKnownAdministrators = ParseSIDMust("S-1-5-32-544")
)

// wellKnownNames maps well-known SID strings to display names.
var wellKnownNames = map[string]string{
	"S-1-1-0":      "Everyone",
	"S-1-3-0":      "CREATOR OWNER",
	"S-1-3-1":      "CREATOR GROUP",
	"S-1-5-7":      "NT AUTHORITY\\ANONYMOUS LOGON",
	"S-1-5-11":     "NT AUTHORITY\\Authenticated Users",
	"S-1-5-18":     "NT AUTHORITY\\SYSTEM",
	"S-1-5-32-544": "BUILTIN\\Administrators",
}

// WellKnownName returns the display name for a well-known SID.
// Returns the name and true if the SID is well-known, or ("", false) otherwise.
func WellKnownName(s *SID) (string, bool) {
	name, ok := wellKnownNames[FormatSID(s)]
	return name, ok
}
