// Package registry loads test/suites.yaml, the list of every test suite, and
// checks it as it loads. The format and its rules are those of
// docs/internals/rfcs/rfc-29-test-harness.md.
package registry

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/test/harness/known"
)

// version is the newest registry format this package reads.
const version = 1

// Path is the registry's location, relative to the repository root.
const Path = "test/suites.yaml"

// QuarantinePath is the quarantine list's location, relative to the
// repository root.
const QuarantinePath = "test/harness/quarantine.yaml"

// ToolDirs maps each test tool {tools} holds to the directory of its output
// that {tools}/NAME links to: pjdfstest's share/pjdfstest holds its binary
// beside its tests, where misc.sh looks for it, pynfs's and nfstest's whole
// outputs hold bin/pynfs-4.x and bin/nfstest_MODULE, and cthon04's
// share/cthon04 is its built tree. A suite names a tool as {tools}/NAME.
var ToolDirs = map[string]string{"cthon04": "share/cthon04", "nfstest": ".", "pjdfstest": "share/pjdfstest", "pynfs": "."}

// ToolCopies are the tools {tools} holds a copy of, writable by the user dt
// runs as, rather than a link to. cthon04's general tests cp their sources to
// the mount, which keeps their modes, so from nix's read-only store each copy
// is a read-only file its owner then writes: what special/op_chmod tests, not
// what general does.
//
// decision: with the copy, general no longer writes to read-only files, so
// special/op_chmod is where the server's handling of that write stays visible.
var ToolCopies = []string{"cthon04"}

// Registry is the whole file.
type Registry struct {
	Version  int                 `yaml:"version"`
	Defaults Suite               `yaml:"defaults"`
	Groups   map[string][]string `yaml:"groups"`
	Suites   Suites              `yaml:"suites"`
	// Known holds the entries of each known file a suite names, by its path,
	// read and checked as the registry loads.
	Known map[string][]known.Entry `yaml:"-"`
	// Quarantine holds the quarantine list's entries for each suite, by its
	// name, as the entries its cells' results are graded by.
	Quarantine map[string][]known.Entry `yaml:"-"`
	// Expired says why each expired entry of the quarantine list excuses
	// nothing, as path:line: why, for dt doctor to report.
	Expired []string `yaml:"-"`
}

// Suites keeps the file's order.
type Suites []*Suite

// Suite is one suite, or one of its options. Every field may also appear in
// the registry's defaults.
type Suite struct {
	Name        string               `yaml:"-"`
	Cmd         Commands             `yaml:"cmd"`
	Dir         string               `yaml:"dir"`
	Format      string               `yaml:"format"`
	Metrics     string               `yaml:"metrics"`
	Tier        string               `yaml:"tier"`
	Blocking    Blocking             `yaml:"blocking"`
	Timeout     Timeout              `yaml:"timeout"`
	Needs       []string             `yaml:"needs"`
	NeedsByTest map[string][]string  `yaml:"needs_by_test"`
	Paths       []string             `yaml:"paths"`
	Tests       Tests                `yaml:"tests"`
	Exclude     []string             `yaml:"exclude"`
	Matrix      Matrix               `yaml:"matrix"`
	Select      map[string]Selection `yaml:"select"`
	Options     Suites               `yaml:"options"`
	Fixture     *Fixture             `yaml:"fixture"`
	Image       string               `yaml:"image"`
	Volumes     map[string]string    `yaml:"volumes"`
	Templates   string               `yaml:"templates"`
	Packages    *Packages            `yaml:"packages"`
	Env         Env                  `yaml:"env"`
	Args        map[string]string    `yaml:"args"`
	Names       string               `yaml:"names"` // what {names} holds when a run names no tests
	Known       string               `yaml:"known"`
	Shards      int                  `yaml:"shards"`
	Job         string               `yaml:"job"`
	Exclusive   bool                 `yaml:"exclusive"` // with --parallel, nothing runs beside its cells
}

// Fixture is the DittoFS server a suite's tests run against.
type Fixture struct {
	Server      string   `yaml:"server"`
	Profile     string   `yaml:"profile"`
	NFS         NFS      `yaml:"nfs"`
	SMB         SMB      `yaml:"smb"`
	Shares      string   `yaml:"shares"`
	Users       []string `yaml:"users"` // UIDs, or self: the user dt runs as
	Squash      string   `yaml:"squash"`
	Delegations *bool    `yaml:"delegations"`
	Lease       string   `yaml:"lease"`
	Kerberos    bool     `yaml:"kerberos"`
	Reset       string   `yaml:"reset"`
	Client      string   `yaml:"client"`
	Clients     int      `yaml:"clients"`
	RPCBind     string   `yaml:"rpcbind"`
}

// Packages selects the Go packages whose tests a build tag changes.
type Packages struct {
	Tag string `yaml:"tag"`
}

// Timeout holds each test's limit and the whole cell's.
type Timeout struct {
	Test string `yaml:"test"`
	Cell string `yaml:"cell"`
}

// Load reads and checks the registry of the repository at root. Every problem
// found is returned, joined, so one run shows them all.
func Load(root string) (*Registry, error) {
	data, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		return nil, err
	}
	return Parse(root, data)
}

// Parse checks data as the registry of the repository at root.
func Parse(root string, data []byte) (*Registry, error) {
	var r Registry
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if errs := r.check(root); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &r, nil
}

// Suite returns the suite called name, or nil.
func (r *Registry) Suite(name string) *Suite {
	for _, s := range r.Suites {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Units returns the suite and each of its options, with the defaults and,
// for an option, the suite's fields filled in.
func (r *Registry) Units(s *Suite) []*Suite {
	base := merge(&r.Defaults, s)
	base.Name, base.Options = s.Name, nil
	units := []*Suite{base}
	for _, o := range s.Options {
		u := merge(base, o)
		u.Name = s.Name + "/" + o.Name
		// A job names the suite's own fixed job, so an option doesn't share it.
		u.Job = o.Job
		// select narrows the matrix, so an option with a matrix of its own
		// doesn't keep the suite's.
		if o.Matrix != nil && o.Select == nil {
			u.Select = nil
		}
		units = append(units, u)
	}
	return units
}

// merge returns a copy of base with each field over sets replaced as a whole,
// except the two timeout keys, which are set separately.
func merge(base, over *Suite) *Suite {
	out := *base
	bv, ov := reflect.ValueOf(&out).Elem(), reflect.ValueOf(over).Elem()
	for i := 0; i < ov.NumField(); i++ {
		if f := ov.Field(i); !f.IsZero() {
			bv.Field(i).Set(f)
		}
	}
	out.Timeout = base.Timeout
	if over.Timeout.Test != "" {
		out.Timeout.Test = over.Timeout.Test
	}
	if over.Timeout.Cell != "" {
		out.Timeout.Cell = over.Timeout.Cell
	}
	return &out
}
