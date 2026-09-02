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
	"slices"
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
	citation = regexp.MustCompile(`\[?(MS-(?:FSCC|FSA|SMB2|DTYP|ERREF))]?\s*(?:[Ss]ection\s*|§\s*)?(\d+(?:\.\d+)+)\s*(?:\("([^"]*)"\))?`)
	// Structure identifiers a comment can name. MS-FSCC and MS-FSA title
	// their per-structure sections with exactly these names, which is what
	// makes the comparison decidable.
	identifiers = []*regexp.Regexp{
		regexp.MustCompile(`\bFile[A-Za-z0-9]*Information(?:Ex)?\b`),
		regexp.MustCompile(`\bFILE_[A-Z0-9_]*INFORMATION\b`),
		regexp.MustCompile(`\bFSCTL_[A-Z0-9_]+\b`),
	}
	structureTitle = regexp.MustCompile(`^(File[A-Za-z0-9]*Information(?:Ex)?|FILE_[A-Z0-9_]+|FSCTL_[A-Z0-9_]+)\b`)
	// Comments wrap at column 80, which regularly leaves the spec name at the
	// end of one line and its section number at the start of the next.
	// Section titles across all five specs use only these characters, so a
	// quoted parenthetical carrying anything else is a quotation from the
	// spec's prose rather than a claim about the section's title.
	titleChars    = regexp.MustCompile(`^[A-Za-z0-9 _./,:()'&-]+$`)
	danglingSpec  = regexp.MustCompile(`\[?MS-(?:FSCC|FSA|SMB2|DTYP|ERREF)]?\s*(?:[Ss]ection|§)?\s*$`)
	commentMarker = regexp.MustCompile(`^\s*(?://+|\*|/\*)\s*`)
)

// structural reports whether a spec titles its sections after the structures
// they define, which is what makes rule 3 decidable.
func structural(spec string) bool { return spec == "MS-FSCC" || spec == "MS-FSA" }

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
	return strings.Join(strings.Fields(b.String()), " ")
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
		for _, nums := range m.byStructure {
			slices.Sort(nums)
		}
		specs[m.Spec] = &m
	}
	return specs, nil
}

// namedStructure returns the single structure identifier a line names, or "".
// Spellings of one structure (FILE_EA_INFORMATION, FileEaInformation) count as
// one; two distinct structures leave the line ambiguous.
func namedStructure(line string) string {
	found, key := "", ""
	for _, re := range identifiers {
		for _, id := range re.FindAllString(line, -1) {
			if found != "" && norm(id) != key {
				return ""
			}
			found, key = id, norm(id)
		}
	}
	return found
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
	n := 0
	for _, h := range hits {
		if structural(h[1]) {
			n++
		}
	}
	id := ""
	if n == 1 {
		id = namedStructure(line)
	}

	var out []finding
	add := func(spec, num, format string, args ...any) {
		out = append(out, finding{spec: spec, num: num, msg: fmt.Sprintf(format, args...)})
	}
	for _, h := range hits {
		spec, num := h[1], h[2]
		m := specs[spec]
		if m == nil {
			continue
		}
		title, ok := m.Sections[num]
		if !ok {
			add(spec, num, "[%s] %s does not exist", spec, num)
			continue
		}
		if claimed := h[3]; titleChars.MatchString(claimed) && norm(claimed) != norm(title) {
			add(spec, num, "[%s] %s is %q, not %q", spec, num, title, claimed)
			continue
		}
		if id == "" || !structural(spec) {
			continue
		}
		defining := m.byStructure[norm(id)]
		if len(defining) == 0 || !structureTitle.MatchString(title) || slices.Contains(defining, num) {
			continue
		}
		add(spec, num, "[%s] %s is %q, but the line names %s, which is %s %s",
			spec, num, title, id, spec, strings.Join(defining, "/"))
	}
	return out
}

// logicalLine is line i with the next line appended when comment wrapping split
// a citation between the two.
func logicalLine(lines []string, i int) string {
	line := lines[i]
	if danglingSpec.MatchString(line) && i+1 < len(lines) {
		line += " " + commentMarker.ReplaceAllString(lines[i+1], "")
	}
	return line
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
	// Nested working copies of this same repository. Their .go files are
	// someone else's in-progress edits, so citations there are neither this
	// tree's problem nor keyed by a path known_wrong.txt can name. Scanning
	// them buries the findings that belong to this tree under hundreds that
	// do not, which is the state in which a check stops being read.
	".claude": true,
	// This check's own fixtures cite the wrong sections on purpose.
	"spec-citations": true,
}

// exitOnErr reports a setup or walk failure as exit 2, distinct from the exit 1
// that means the check ran and found problems.
func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec-citations:", err)
		os.Exit(2)
	}
}

// scanTree walks root and reports every citation problem in it, plus the number
// of Go files read. known is updated in place: an entry it matched is marked
// seen, so the caller can report the ones that no longer match anything.
func scanTree(root string, specs map[string]*specMap, known map[string]bool) ([]finding, int, error) {
	var findings []finding
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		// known_wrong.txt spells its paths with forward slashes, so the key
		// has to be separator-independent.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		lines := strings.Split(string(raw), "\n")
		for i := range lines {
			for _, f := range checkLine(specs, logicalLine(lines, i)) {
				f.file, f.line = rel, i+1
				k := f.key()
				if _, ok := known[k]; ok {
					known[k] = true
					continue
				}
				findings = append(findings, f)
			}
		}
		return nil
	})
	return findings, scanned, err
}

func main() {
	root := flag.String("root", ".", "directory tree to scan")
	flag.Parse()

	specs, err := loadSpecs()
	exitOnErr(err)
	known, err := loadKnownWrong()
	exitOnErr(err)

	findings, scanned, err := scanTree(*root, specs, known)
	exitOnErr(err)

	for _, f := range findings {
		fmt.Printf("%s:%d: %s (%s %s)\n", f.file, f.line, f.msg, f.spec, specs[f.spec].Revision)
	}
	var stale []string
	for k, seen := range known {
		if !seen {
			stale = append(stale, k)
		}
	}
	slices.Sort(stale)
	for _, k := range stale {
		fmt.Printf("known_wrong.txt: %q no longer matches any citation; delete the entry\n", k)
	}
	if n := len(findings) + len(stale); n > 0 {
		fmt.Fprintf(os.Stderr, "\nspec-citations: %d problem(s) across %d Go files\n", n, scanned)
		os.Exit(1)
	}
	fmt.Printf("spec-citations: %d Go files clean (%d known-wrong citation(s) allowed)\n", scanned, len(known))
}
