package main

import (
	"fmt"
	"regexp"

	"github.com/marmos91/dittofs/bench/workloads"
)

// knownWorkloads are the fio job files shipped in bench/workloads. Each maps to
// <name>.fio. Kept as an explicit list (not a dir scan) so --workloads
// selection and `list` have a stable, ordered set.
var knownWorkloads = []string{
	"seq-write",
	"seq-read",
	"rand-write-4k",
	"rand-read-4k",
	"mixed-rw",
	"metadata",
}

// sizeClasses map a friendly size name to the fio --size value. Explicit sizes
// (e.g. "512m") are also accepted and passed through verbatim.
var sizeClasses = map[string]string{
	"small":  "64k",
	"medium": "1m",
	"large":  "1g",
}

func sizeClassOrder() []string { return []string{"small", "medium", "large"} }

// resolveSize returns the fio --size string for a size selector, accepting
// either a named class or an explicit fio size literal.
func resolveSize(sel string) string {
	if v, ok := sizeClasses[sel]; ok {
		return v
	}
	return sel
}

// jobDefaults are the ${VAR:-default} substitutions applied to a .fio template.
// The .fio files use bash-style default syntax that fio itself does not
// understand, so we expand them before handing the file to fio. dir/size are
// expanded here rather than passed as fio CLI flags because a job-file option
// (directory=/size=) overrides a global --directory/--size, so CLI overrides
// silently don't take. An empty size falls back to the template default.
func jobDefaults(engine, dir, size string, threads, runtime int) map[string]string {
	p := map[string]string{
		"FIO_ENGINE":    engine,
		"BENCH_THREADS": fmt.Sprintf("%d", threads),
		"BENCH_RUNTIME": fmt.Sprintf("%d", runtime),
		"BENCH_DIR":     dir,
	}
	if size != "" {
		p["BENCH_SIZE"] = size
	}
	return p
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandJob renders a .fio template, replacing ${VAR} and ${VAR:-default} with
// values from params, falling back to the template's own default. Unset vars
// with no default expand to empty (matching shell).
func expandJob(tmpl string, params map[string]string) string {
	return varRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		g := varRe.FindStringSubmatch(m)
		name, def := g[1], g[2]
		if v, ok := params[name]; ok {
			return v
		}
		return def
	})
}

// loadJob returns the expanded fio job file body for a workload.
func loadJob(name string, params map[string]string) (string, error) {
	raw, err := workloads.FS.ReadFile(name + ".fio")
	if err != nil {
		return "", fmt.Errorf("unknown workload %q: %w", name, err)
	}
	return expandJob(string(raw), params), nil
}

// validWorkload reports whether name is a shipped workload.
func validWorkload(name string) bool {
	for _, w := range knownWorkloads {
		if w == name {
			return true
		}
	}
	return false
}
