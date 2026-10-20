package fixture

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// shareFlags sets, for each dfsctl share create flag a shares file may name,
// what the flag sets in the share's request, and says whether the value is of
// the flag's kind. They are the flags the suites' shares use: true or false,
// and default-permission's level.
var shareFlags = map[string]func(r *apiclient.CreateShareRequest, v any) bool{
	"encrypt-data":               onOff(func(r *apiclient.CreateShareRequest, on bool) { r.EncryptData = on }),
	"access-based-enumeration":   onOff(func(r *apiclient.CreateShareRequest, on bool) { r.AccessBasedEnumeration = &on }),
	"acl-canonicalize-inherited": onOff(func(r *apiclient.CreateShareRequest, on bool) { r.AclFlagInheritedCanonicalization = &on }),
	"change-notify-disabled":     onOff(func(r *apiclient.CreateShareRequest, on bool) { r.ChangeNotifyDisabled = &on }),
	"streams-disabled":           onOff(func(r *apiclient.CreateShareRequest, on bool) { r.StreamsDisabled = &on }),
	"continuous-availability":    onOff(func(r *apiclient.CreateShareRequest, on bool) { r.ContinuousAvailability = &on }),
	"default-permission": func(r *apiclient.CreateShareRequest, v any) bool {
		r.DefaultPermission, _ = v.(string)
		return r.DefaultPermission != ""
	},
}

// onOff is a flag that is true or false.
func onOff(set func(r *apiclient.CreateShareRequest, on bool)) func(r *apiclient.CreateShareRequest, v any) bool {
	return func(r *apiclient.CreateShareRequest, v any) bool {
		on, ok := v.(bool)
		set(r, on)
		return ok
	}
}

// loadShares reads the shares file at path, a list of shares, each with its
// name and the flags dfsctl share create would take for it, by the flag's
// name. Without a file the fixture has the one default share.
func loadShares(path string) ([]apiclient.CreateShareRequest, error) {
	if path == "" {
		return []apiclient.CreateShareRequest{{Name: Share}}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	if err := yaml.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s holds no share", path)
	}
	shares := make([]apiclient.CreateShareRequest, len(list))
	for i, entry := range list {
		name, _ := entry["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("%s: share %d has no name", path, i+1)
		}
		shares[i].Name = name
		for flag, v := range entry {
			if flag == "name" {
				continue
			}
			set, known := shareFlags[flag]
			if !known || !set(&shares[i], v) {
				return nil, fmt.Errorf("%s: share %s: %s isn't a flag dt sets, to a value of its kind", path, name, flag)
			}
		}
	}
	return shares, nil
}
