package run

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fill turns a command into the arguments it runs with, its placeholders
// replaced. A line is split at spaces and never passes through a shell, and
// a whole {args} or {packages}, which hold lists, becomes one argument per
// item; an sh command runs through sh -c, with its values shell-quoted.
func fill(c registry.Command, vals map[string]string, lists map[string][]string) ([]string, error) {
	if c.Sh != "" {
		s, err := replace(c.Sh, vals, lists, shellQuote)
		return []string{"sh", "-c", s}, err
	}
	var argv []string
	for _, field := range strings.Fields(c.Line) {
		if name, whole := strings.CutPrefix(field, "{"); whole && strings.HasSuffix(name, "}") {
			if items, ok := lists[strings.TrimSuffix(name, "}")]; ok {
				argv = append(argv, items...)
				continue
			}
		}
		s, err := replace(field, vals, lists, nil)
		if err != nil {
			return nil, err
		}
		argv = append(argv, s)
	}
	if len(argv) == 0 {
		return nil, errors.New("a command is empty")
	}
	return argv, nil
}

// replace fills the placeholders in s, each value through quote when it's
// given; a list's items are joined with spaces.
func replace(s string, vals map[string]string, lists map[string][]string, quote func(string) string) (string, error) {
	if quote == nil {
		quote = func(v string) string { return v }
	}
	var missing []string
	out := registry.PlaceholderRE.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "$") {
			return m
		}
		name := m[1 : len(m)-1]
		if items, ok := lists[name]; ok {
			quoted := make([]string, len(items))
			for i, item := range items {
				quoted[i] = quote(item)
			}
			return strings.Join(quoted, " ")
		}
		v, ok := vals[name]
		if !ok {
			missing = append(missing, "{"+name+"}")
			return m
		}
		return quote(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("dt doesn't provide %s yet", strings.Join(missing, " "))
	}
	return out, nil
}

// shellQuote quotes v as one word for sh.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// suiteEnv is the suite's environment, for every tier and then for its own,
// which wins.
func suiteEnv(u *registry.Suite, tier string, vals map[string]string, lists map[string][]string) ([]string, error) {
	var env []string
	for _, m := range []map[string]string{u.Env.All, u.Env.ByTier[tier]} {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			v, err := replace(m[k], vals, lists, nil)
			if err != nil {
				return nil, err
			}
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}
