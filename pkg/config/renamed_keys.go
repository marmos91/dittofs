package config

import (
	"fmt"
	"sort"
	"strings"
)

// renamedConfigKeys maps the dotted prefix of a config section that moved to
// the prefix that replaced it. Entries stay until a release nobody upgrades
// across still carried the old name.
var renamedConfigKeys = map[string]string{
	"blockstore.local": "blockstore.journal",
}

// checkRenamedKeys reports keys the config file set that have since been
// renamed.
//
// Unknown keys are otherwise only warned about, so that an upgrade does not
// hard-fail on a config still carrying a key from a deleted section. A renamed
// key is the one case that cannot be treated that way: the operator believes
// the setting is in effect, an equivalent still exists under a new name, and
// ignoring it changes runtime behaviour with nothing to show for it. Naming
// both halves and refusing is recoverable; booting with a silently dropped
// setting is not.
//
// unused holds the keys the decoder could not place, as the file spelled them.
func checkRenamedKeys(unused []string) error {
	var found []string
	for _, key := range unused {
		lower := strings.ToLower(key)
		for old, replacement := range renamedConfigKeys {
			if lower == old || strings.HasPrefix(lower, old+".") {
				suffix := strings.TrimPrefix(lower, old)
				found = append(found, fmt.Sprintf("%s -> %s%s", lower, replacement, suffix))
				break
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("config uses renamed keys; update them and restart:\n  %s",
		strings.Join(found, "\n  "))
}
