package backend

import "context"

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
