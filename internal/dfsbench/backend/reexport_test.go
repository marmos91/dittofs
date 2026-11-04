package backend

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/dfsbench/fio"
)

func TestSambaReexportConfig(t *testing.T) {
	const source = "/mnt/bench source"
	conf := fio.ExpandJob(smbConfTmpl, map[string]string{"SHARE": sambaShare, "SRC_PATH": source})

	// Read the active settings from the rendered template, excluding comments
	// and tracking sections so a global setting cannot stand in for a share.
	settings := make(map[string]string)
	section := ""
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Fatalf("invalid rendered config line %q", line)
		}
		settings[section+"/"+strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	// The loopback client mounts this named guest share over SMB3. Its
	// durability tier relies on honoring flushes without syncing every write.
	want := map[string]string{
		"global/server min protocol": "SMB3_00",
		"global/map to guest":        "Bad User",
		sambaShare + "/path":         source,
		sambaShare + "/read only":    "no",
		sambaShare + "/guest ok":     "yes",
		sambaShare + "/strict sync":  "yes",
		sambaShare + "/sync always":  "no",
	}
	for key, value := range want {
		if got := settings[key]; got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}
