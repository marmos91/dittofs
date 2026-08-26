// Command spec-citations checks Open Specifications citations in Go source
// against a vendored section-number -> title map for each spec.
//
// Three rules fire per citation:
//
//  1. the cited section number is absent from the spec's section map;
//  2. the citation carries a quoted parenthetical title that is not the
//     cited section's title;
//  3. the line names exactly one structure identifier (File*Information,
//     FILE_*_INFORMATION, FSCTL_*), the spec has a section defining that
//     structure, and the cited section defines a different one.
//
// Rule 3 stays silent when the cited section's title is prose rather than a
// structure name: citing an algorithm section from a line that happens to name
// a structure is ordinary and correct. Family references (§2.4), byte-size
// parentheticals and everything else ambiguous are out of scope by
// construction — the check only speaks when the source has already stated the
// answer and contradicts itself. Whether the section's prose supports the
// claim beside it is not decidable here and is not attempted.
//
// Citations known to be wrong and not yet corrected are listed in
// known_wrong.txt; an entry that stops matching fails the run, so the list
// cannot rot.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed sections/*.json known_wrong.txt
var vendored embed.FS

type specMap struct {
	Spec     string            `json:"spec"`
	Revision string            `json:"revision"`
	Sections map[string]string `json:"sections"`

	// byStructure maps a normalized structure name to the sections whose
	// title defines it, e.g. "fsctlsetcompression" -> 2.3.44, 2.3.45.
	byStructure map[string][]string
}

var (
	citation = regexp.MustCompile(`\[?(MS-(?:FSCC|FSA|SMB2|DTYP|ERREF))]?\s*(?:[Ss]ection\s*|§\s*)?(\d+(?:\.\d+)+)\s*(\("[^"]*"\))?`)
	// Structure identifiers a comment can name. MS-FSCC and MS-FSA title
	// their per-structure sections with exactly these names, which is what
	// makes the comparison decidable.
	identifiers = []*regexp.Regexp{
		regexp.MustCompile(`\bFile[A-Za-z0-9]*Information(?:Ex)?\b`),
		regexp.MustCompile(`\bFILE_[A-Z0-9_]*INFORMATION\b`),
		regexp.MustCompile(`\bFSCTL_[A-Z0-9_]+\b`),
	}
	structureTitle = regexp.MustCompile(`^(File[A-Za-z0-9]*Information(?:Ex)?|FILE_[A-Z0-9_]+|FSCTL_[A-Z0-9_]+)\b`)
	spaces         = regexp.MustCompile(`\s+`)
)

// norm lowercases and drops everything but letters, digits and single spaces,
// so that FILE_FULL_EA_INFORMATION and FileFullEaInformation compare equal
// while "FSCTL_SET_COMPRESSION Request" keeps the word boundary that makes it a
// prefix of, and not equal to, the bare FSCTL name.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			b.WriteRune(' ')
		}
	}
	return strings.TrimSpace(spaces.ReplaceAllString(b.String(), " "))
}

func loadSpecs() (map[string]*specMap, error) {
	entries, err := vendored.ReadDir("sections")
	if err != nil {
		return nil, err
	}
	specs := make(map[string]*specMap, len(entries))
	for _, e := range entries {
		raw, err := vendored.ReadFile("sections/" + e.Name())
		if err != nil {
			return nil, err
		}
		var m specMap
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		m.byStructure = map[string][]string{}
		for num, title := range m.Sections {
			if !structureTitle.MatchString(title) {
				continue
			}
			// The structure a title defines is the name it opens with; the
			// rest is a qualifier some titles carry ("... Request", "... for
			// the SMB2 Protocol") and the identifier in a comment omits.
			key := norm(structureTitle.FindString(title))
			m.byStructure[key] = append(m.byStructure[key], num)
		}
		for k := range m.byStructure {
			sort.Strings(m.byStructure[k])
		}
		specs[m.Spec] = &m
	}
	return specs, nil
}

// namedStructure returns the single structure identifier a line names, or "".
func namedStructure(line string) string {
	seen := map[string]string{}
	for _, re := range identifiers {
		for _, id := range re.FindAllString(line, -1) {
			seen[norm(id)] = id
		}
	}
	if len(seen) != 1 {
		return ""
	}
	for _, id := range seen {
		return id
	}
	return ""
}

type finding struct {
	file string
	line int
	spec string
	num  string
	msg  string
}

// key identifies a citation for known_wrong.txt. It deliberately omits the
// line number and the message, so that moving code or bumping a spec revision
// does not invalidate the entry.
func (f finding) key() string { return fmt.Sprintf("%s: %s %s", f.file, f.spec, f.num) }

func checkLine(specs map[string]*specMap, line string) []finding {
	hits := citation.FindAllStringSubmatch(line, -1)
	if len(hits) == 0 {
		return nil
	}
	// Binding an identifier to a citation is only unambiguous when the line
	// carries a single structure-titled citation.
	structural := 0
	for _, h := range hits {
		if h[1] == "MS-FSCC" || h[1] == "MS-FSA" {
			structural++
		}
	}
	id := ""
	if structural == 1 {
		id = namedStructure(line)
	}

	var out []finding
	add := func(spec, num, format string, args ...any) {
		out = append(out, finding{spec: spec, num: num, msg: fmt.Sprintf(format, args...)})
	}
	for _, h := range hits {
		spec, num, paren := h[1], h[2], h[3]
		m := specs[spec]
		if m == nil {
			continue
		}
		title, ok := m.Sections[num]
		if !ok {
			add(spec, num, "[%s] %s does not exist", spec, num)
			continue
		}
		if paren != "" {
			claimed := strings.Trim(paren, `("")`)
			if norm(claimed) != norm(title) {
				add(spec, num, "[%s] %s is %q, not %q", spec, num, title, claimed)
				continue
			}
		}
		if id == "" || (spec != "MS-FSCC" && spec != "MS-FSA") {
			continue
		}
		defining := m.byStructure[norm(id)]
		if len(defining) == 0 || !structureTitle.MatchString(title) {
			continue
		}
		cited := false
		for _, d := range defining {
			if d == num {
				cited = true
				break
			}
		}
		if cited {
			continue
		}
		add(spec, num, "[%s] %s is %q, but the line names %s, which is %s %s",
			spec, num, title, id, spec, strings.Join(defining, "/"))
	}
	return out
}

func loadKnownWrong() (map[string]bool, error) {
	raw, err := vendored.ReadFile("known_wrong.txt")
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		known[line] = false
	}
	return known, nil
}

var skipDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true,
	"graphify-out": true, ".planning": true,
}

func main() {
	root := flag.String("root", ".", "directory tree to scan")
	flag.Parse()

	specs, err := loadSpecs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec-citations:", err)
		os.Exit(2)
	}
	known, err := loadKnownWrong()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec-citations:", err)
		os.Exit(2)
	}

	var findings []finding
	scanned := 0
	err = filepath.WalkDir(*root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		rel, relErr := filepath.Rel(*root, path)
		if relErr != nil {
			rel = path
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, f := range checkLine(specs, line) {
				f.file, f.line = rel, i+1
				if _, ok := known[f.key()]; ok {
					known[f.key()] = true
					continue
				}
				findings = append(findings, f)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec-citations:", err)
		os.Exit(2)
	}

	for _, f := range findings {
		fmt.Printf("%s:%d: %s (%s %s)\n", f.file, f.line, f.msg, f.spec, specs[f.spec].Revision)
	}
	var stale []string
	for k, seen := range known {
		if !seen {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		fmt.Printf("known_wrong.txt: %q no longer matches any citation; delete the entry\n", k)
	}
	if n := len(findings) + len(stale); n > 0 {
		fmt.Fprintf(os.Stderr, "\nspec-citations: %d problem(s) across %d Go files\n", n, scanned)
		os.Exit(1)
	}
	fmt.Printf("spec-citations: %d Go files clean (%d known-wrong citation(s) allowed)\n", scanned, len(known))
}
