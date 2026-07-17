package fio

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/marmos91/dittofs/bench/workloads"
)

// SizeBytes parses an fio size string ("64k", "1m", "1g", "512M", or plain
// bytes) into a byte count; returns 0 if unparseable. Used to size the read
// target's durability barrier.
func SizeBytes(s string) int64 {
	if s == "" {
		return 0
	}
	mult := int64(1)
	num := s
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, num = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, num = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, num = 1<<30, s[:len(s)-1]
	case 't', 'T':
		mult, num = 1<<40, s[:len(s)-1]
	}
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0
	}
	return v * mult
}

// KnownWorkloads are the fio job files shipped in bench/workloads. Each maps to
// <name>.fio. Kept as an explicit list (not a dir scan) so --workloads
// selection and `list` have a stable, ordered set.
//
// metadata runs FIRST, before any write-heavy workload (#1739). The warm pass
// runs every workload sequentially against one live server/mount, so if metadata
// ran last it would measure creates while the block syncer is still draining the
// GBs of async S3 backlog that seq-write/rand-write/mixed queued — IO-wait
// contention that, on a full matrix's accumulated backlog, degrades the create
// cell into a near-hang (the "8 ops in 240s" garbage). Measuring metadata on a
// quiesced store isolates the metadata engine, which is what this cell is for.
var KnownWorkloads = []string{
	"metadata",
	"seq-write",
	"seq-write-buffered",
	"seq-read",
	"rand-write-4k",
	"rand-read-4k",
	"mixed-rw",
}

// SizeClasses map a friendly size name to the fio --size value. Explicit sizes
// (e.g. "512m") are also accepted and passed through verbatim.
var SizeClasses = map[string]string{
	"small":  "64k",
	"medium": "1m",
	"large":  "1g",
}

// SizeClassOrder is the display order for the named size classes.
func SizeClassOrder() []string { return []string{"small", "medium", "large"} }

// ResolveSize returns the fio --size string for a size selector, accepting
// either a named class or an explicit fio size literal.
func ResolveSize(sel string) string {
	if v, ok := SizeClasses[sel]; ok {
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

// ExpandJob renders a .fio template, replacing ${VAR} and ${VAR:-default} with
// values from params, falling back to the template's own default. Unset vars
// with no default expand to empty (matching shell).
func ExpandJob(tmpl string, params map[string]string) string {
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
	return ExpandJob(string(raw), params), nil
}

// ValidWorkload reports whether name is a shipped workload.
func ValidWorkload(name string) bool {
	for _, w := range KnownWorkloads {
		if w == name {
			return true
		}
	}
	return false
}
