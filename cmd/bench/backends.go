package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Protocol is how a client mounts a backend. "local" (PR2) means fio runs
// directly against a supplied mount; the managed protocols below are what the
// harness itself exports and mounts per cell.
type Protocol string

const (
	ProtoNFS3 Protocol = "nfs3"
	ProtoNFS4 Protocol = "nfs4"
	ProtoSMB3 Protocol = "smb3"
)

// managedProtocols is the fixed export order the matrix iterates.
var managedProtocols = []Protocol{ProtoNFS3, ProtoNFS4, ProtoSMB3}

// Support marks, per (backend, protocol), how the backend is exported — the
// capability map the plan calls for. Native means the backend serves the
// protocol itself (DittoFS's design point); Reexport means a FUSE mount is
// re-served over knfsd/Samba (the FUSE tax we flag in the report); NA means the
// combo is invalid and auto-skips.
type Support int

const (
	NA Support = iota
	Native
	Reexport
)

func (s Support) String() string {
	switch s {
	case Native:
		return "native"
	case Reexport:
		return "reexport"
	default:
		return "na"
	}
}

// Backend is one system-under-test. Lifecycle recipes are func fields (not an
// interface — there is exactly one impl per backend, and func fields make the
// registry a flat table and fakes trivial in tests). Recipes shell out to the
// VM's CLIs and only run on Linux; the registry/resolution logic is pure Go.
type Backend struct {
	Name     string
	S3Backed bool
	Support  map[Protocol]Support

	// Setup installs+starts the backend (idempotent); Mount exports it over
	// proto and returns the client mount dir; Evict forces the next read
	// cold-from-S3; Unmount/Teardown reverse Mount/Setup. Any may be nil while
	// a recipe is still unimplemented — the runner skips a plan whose Support is
	// non-NA but whose recipes are missing, so partial registries stay runnable.
	Setup    func(ctx context.Context, env BackendEnv) error
	Mount    func(ctx context.Context, proto Protocol) (mnt string, err error)
	Evict    func(ctx context.Context) error
	Unmount  func(ctx context.Context, proto Protocol) error
	Teardown func(ctx context.Context) error
}

// BackendEnv carries the per-run knobs a Setup recipe needs (S3 creds stay in
// the process environment per the plan invariant — not passed here).
type BackendEnv struct {
	Bucket   string
	Endpoint string
}

// registry holds every registered backend by name. Adding a competitor is one
// register() call (plan: "add a competitor = 1 registry line + 1 setup script").
var registry = map[string]*Backend{}

func register(b *Backend) {
	if _, dup := registry[b.Name]; dup {
		panic("bench: duplicate backend " + b.Name)
	}
	registry[b.Name] = b
}

// plan is a resolved (backend, protocol) target the matrix expands over passes.
type plan struct {
	backend  *Backend
	protocol Protocol
	support  Support
}

// systemLabel is the cell's System string, e.g. "dittofs-s3-nfs3".
func (p plan) systemLabel() string { return p.backend.Name + "-" + string(p.protocol) }

// resolveSystems turns --systems labels into concrete (backend, protocol)
// plans. A bare backend name expands to every protocol it supports; a
// "backend-proto" label pins one protocol. NA combos are rejected when named
// explicitly and skipped when they fall out of a bare-name expansion.
func resolveSystems(labels []string) ([]plan, error) {
	var plans []plan
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
			plans = append(plans, plan{backend: b, protocol: proto, support: sup})
			continue
		}
		for _, proto := range managedProtocols {
			if sup := b.Support[proto]; sup != NA {
				plans = append(plans, plan{backend: b, protocol: proto, support: sup})
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

// backendNames returns registered backend names, sorted, for `list`.
func backendNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
