package registry

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Command is one command of a suite: argument words split at spaces, or a
// line run through sh -c.
type Command struct {
	Line string
	Sh   string
}

// Commands is a single command or a list of them, run in order.
type Commands []Command

// Env holds variables for every tier, and per tier under the tier's name.
type Env struct {
	All    map[string]string
	ByTier map[string]map[string]string
}

// Blocking is false or true for every tier, or set per tier.
type Blocking struct {
	All    *bool
	ByTier map[string]bool
}

// Tests is a glob with one test per matching file, a list of names, or a
// .txt file of names.
type Tests struct {
	Glob  string
	Names []string
	File  string
}

// Set reports whether the suite names its tests.
func (t Tests) Set() bool { return t.Glob != "" || t.File != "" || len(t.Names) > 0 }

// Axis is one matrix axis and its values.
type Axis struct {
	Name   string
	Values []string
}

// Matrix keeps its axes in the file's order, which names each cell.
type Matrix []Axis

// Selection narrows one tier: a glob over the suite's tests, or the matrix
// values that tier runs.
type Selection struct {
	Glob   string
	Values map[string][]string
}

// NFS is a version to mount at, or a version with mount options.
type NFS struct {
	Version string `yaml:"version"`
	Options string `yaml:"options"`
}

// SMB enables the SMB adapter, optionally with encryption or a mount.
type SMB struct {
	Enabled    bool   `yaml:"-"`
	Encryption string `yaml:"encryption"`
	Mount      bool   `yaml:"mount"`
}

func (r *Registry) UnmarshalYAML(n *yaml.Node) error {
	type plain Registry
	return decodeStrict(n, (*plain)(r))
}

func (s *Suite) UnmarshalYAML(n *yaml.Node) error {
	type plain Suite
	return decodeStrict(n, (*plain)(s))
}

func (f *Fixture) UnmarshalYAML(n *yaml.Node) error {
	type plain Fixture
	return decodeStrict(n, (*plain)(f))
}

func (p *Packages) UnmarshalYAML(n *yaml.Node) error {
	type plain Packages
	return decodeStrict(n, (*plain)(p))
}

func (t *Timeout) UnmarshalYAML(n *yaml.Node) error {
	type plain Timeout
	return decodeStrict(n, (*plain)(t))
}

func (s *Suites) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping of names", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		suite := &Suite{}
		if err := n.Content[i+1].Decode(suite); err != nil {
			return err
		}
		suite.Name = n.Content[i].Value
		*s = append(*s, suite)
	}
	return nil
}

func (c *Commands) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		cmd, err := decodeCommand(n)
		*c = Commands{cmd}
		return err
	}
	for _, item := range n.Content {
		cmd, err := decodeCommand(item)
		if err != nil {
			return err
		}
		*c = append(*c, cmd)
	}
	return nil
}

func decodeCommand(n *yaml.Node) (Command, error) {
	if n.Kind == yaml.ScalarNode {
		return Command{Line: n.Value}, nil
	}
	var sh struct {
		Sh string `yaml:"sh"`
	}
	if err := decodeStrict(n, &sh); err != nil {
		return Command{}, err
	}
	return Command{Sh: sh.Sh}, nil
}

func (e *Env) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: env is a mapping", n.Line)
	}
	e.All = map[string]string{}
	for i := 0; i < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		if val.Kind != yaml.MappingNode {
			e.All[key] = val.Value
			continue
		}
		if !isTier(key) {
			return fmt.Errorf("line %d: env %q: only a tier name takes a mapping", val.Line, key)
		}
		vars := map[string]string{}
		if err := val.Decode(&vars); err != nil {
			return err
		}
		if e.ByTier == nil {
			e.ByTier = map[string]map[string]string{}
		}
		e.ByTier[key] = vars
	}
	return nil
}

func (b *Blocking) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v, err := strconv.ParseBool(n.Value)
		if err != nil {
			return fmt.Errorf("line %d: blocking is true, false or per tier", n.Line)
		}
		b.All = &v
		return nil
	}
	return n.Decode(&b.ByTier)
}

func (t *Tests) UnmarshalYAML(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.SequenceNode:
		return n.Decode(&t.Names)
	case strings.HasSuffix(n.Value, ".txt"):
		t.File = n.Value
	default:
		t.Glob = n.Value
	}
	return nil
}

func (m *Matrix) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: matrix is a mapping of axes", n.Line)
	}
	*m = Matrix{}
	for i := 0; i < len(n.Content); i += 2 {
		axis := Axis{Name: n.Content[i].Value}
		if err := n.Content[i+1].Decode(&axis.Values); err != nil {
			return err
		}
		*m = append(*m, axis)
	}
	return nil
}

func (s *Selection) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Glob = n.Value
		return nil
	}
	return n.Decode(&s.Values)
}

func (v *NFS) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v.Version = n.Value
		return nil
	}
	type plain NFS
	return decodeStrict(n, (*plain)(v))
}

func (v *SMB) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		on, err := strconv.ParseBool(n.Value)
		v.Enabled = on
		return err
	}
	type plain SMB
	v.Enabled = true
	return decodeStrict(n, (*plain)(v))
}

// decodeStrict decodes a mapping into v, refusing a key v has no field for,
// so a misspelt field fails rather than being ignored. A custom unmarshaler
// gets a plain decoder, so every struct type checks its own keys here.
func decodeStrict(n *yaml.Node, v any) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", n.Line)
	}
	known := map[string]bool{}
	t := reflect.TypeOf(v).Elem()
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" && name != "-" {
			known[name] = true
		}
	}
	for i := 0; i < len(n.Content); i += 2 {
		if key := n.Content[i]; !known[key.Value] {
			return fmt.Errorf("line %d: unknown field %q", key.Line, key.Value)
		}
	}
	return n.Decode(v)
}
