package backend

import (
	"fmt"
	"sort"
	"strings"
)

// registry holds every registered backend by name. Adding a competitor is one
// register() call (plan: "add a competitor = 1 registry line + 1 setup script").
var registry = map[string]*Backend{}

func register(b *Backend) {
	if _, dup := registry[b.Name]; dup {
		panic("bench: duplicate backend " + b.Name)
	}
	registry[b.Name] = b
}

// ResolveSystems turns --systems labels into concrete (backend, protocol)
// plans. A bare backend name expands to every protocol it supports; a
// "backend-proto" label pins one protocol. NA combos are rejected when named
// explicitly and skipped when they fall out of a bare-name expansion.
func ResolveSystems(labels []string) ([]Plan, error) {
	var plans []Plan
	for _, label := range labels {
		b, proto, explicit, err := splitSystemLabel(label)
		if err != nil {
			return nil, err
		}
		if explicit {
			sup := b.Support[proto]
			if sup == NA {
				return nil, fmt.Errorf("system %q: backend %q does not support %s", label, b.Name, proto)
			}
			plans = append(plans, Plan{Backend: b, Protocol: proto, Support: sup})
			continue
		}
		for _, proto := range managedProtocols {
			if sup := b.Support[proto]; sup != NA {
				plans = append(plans, Plan{Backend: b, Protocol: proto, Support: sup})
			}
		}
	}
	return plans, nil
}

// splitSystemLabel parses "backend" or "backend-proto". Backend names may
// contain hyphens (e.g. "dittofs-s3"), so we peel a known protocol suffix
// rather than splitting on the first hyphen.
func splitSystemLabel(label string) (b *Backend, proto Protocol, explicit bool, err error) {
	for _, p := range managedProtocols {
		if strings.HasSuffix(label, "-"+string(p)) {
			name := strings.TrimSuffix(label, "-"+string(p))
			bk, ok := registry[name]
			if !ok {
				return nil, "", false, fmt.Errorf("unknown backend %q (in system %q)", name, label)
			}
			return bk, p, true, nil
		}
	}
	bk, ok := registry[label]
	if !ok {
		return nil, "", false, fmt.Errorf("unknown backend %q; see `dfsbench list`", label)
	}
	return bk, "", false, nil
}

// backendProtos formats a backend's supported protocols for `list`, e.g.
// "nfs3(reexport) nfs4(reexport)".
func backendProtos(b *Backend) string {
	var parts []string
	for _, p := range managedProtocols {
		if sup := b.Support[p]; sup != NA {
			parts = append(parts, fmt.Sprintf("%s(%s)", p, sup))
		}
	}
	return strings.Join(parts, " ")
}

// backendNames returns registered backend names, sorted, for `list`.
func backendNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
