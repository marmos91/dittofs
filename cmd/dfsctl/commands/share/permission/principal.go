package permission

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// principalKind classifies the target of a grant/revoke after resolving the
// --user / --group / --sid flags.
type principalKind int

const (
	principalUserName  principalKind = iota // a local-or-directory user name
	principalGroupName                      // a local-or-directory group name
	principalSID                            // a raw Windows SID
)

// isSIDLiteral reports whether v is a syntactically valid Windows SID string.
func isSIDLiteral(v string) bool {
	_, err := sid.ParseSIDString(v)
	return err == nil
}

// selectPrincipal validates that exactly one of user/group/sid is set and
// classifies the target, shared by the grant and revoke commands. A --user or
// --group value that is itself a SID literal is treated as a SID grant (isGroup
// reflecting the user/group intent); an explicit --sid is a group-form SID.
// target is a human-readable label for command output.
func selectPrincipal(user, group, sidFlag string) (kind principalKind, value string, isGroup bool, target string, err error) {
	set := 0
	for _, v := range []string{user, group, sidFlag} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		return 0, "", false, "", fmt.Errorf("one of --user, --group, or --sid must be specified")
	}
	if set > 1 {
		return 0, "", false, "", fmt.Errorf("--user, --group, and --sid are mutually exclusive")
	}

	switch {
	case sidFlag != "":
		if !isSIDLiteral(sidFlag) {
			return 0, "", false, "", fmt.Errorf("invalid SID %q", sidFlag)
		}
		return principalSID, sidFlag, true, fmt.Sprintf("SID '%s'", sidFlag), nil
	case user != "":
		if isSIDLiteral(user) {
			return principalSID, user, false, fmt.Sprintf("user SID '%s'", user), nil
		}
		return principalUserName, user, false, fmt.Sprintf("user '%s'", user), nil
	default: // group
		if isSIDLiteral(group) {
			return principalSID, group, true, fmt.Sprintf("group SID '%s'", group), nil
		}
		return principalGroupName, group, true, fmt.Sprintf("group '%s'", group), nil
	}
}
