package main

// helpref_test.go checks the command references that appear inside the CLI's own
// prose against the command trees they describe.
//
// cli.md is generated, so it cannot drift. Everything else can: a Long
// description, an Example block, a log line or an error message that tells the
// user to run something is a plain string, and nothing stops it from naming a
// subcommand that moved or a flag that was renamed. A user following one of
// those gets "unknown command" and no way to tell whether the feature exists.
//
// The check is deliberately narrow so it never blocks a legitimate string. A
// token is only required to be a subcommand when the command it would attach to
// has subcommands of its own — those group commands take no positional
// arguments, so anything in that slot is either a child or a mistake. At a leaf
// the walk stops and only flags are still checked.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"

	dfs "github.com/marmos91/dittofs/cmd/dfs/commands"
	dfsctl "github.com/marmos91/dittofs/cmd/dfsctl/commands"
)

// roots maps a binary name to its command tree.
func roots() map[string]*cobra.Command {
	return map[string]*cobra.Command{
		"dfs":    dfs.GetRootCmd(),
		"dfsctl": dfsctl.GetRootCmd(),
	}
}

// placeholderish reports whether tok is standing in for a value the user
// supplies rather than naming a command: an angle/square/brace placeholder, a
// path, a quoted string, an env-style capital, a shell operator. Reaching one
// ends the command-path walk.
func placeholderish(tok string) bool {
	if tok == "" {
		return true
	}
	if strings.ContainsAny(tok, `<>[]{}()/\|&;$"'*=:,%@!?~`) {
		return true
	}
	// Anything outside ASCII is prose or a typographic placeholder ("…"), never
	// a command name.
	for _, r := range tok {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return strings.ToLower(tok) != tok
}

// invocation is one `dfs …` / `dfsctl …` reference found in a string.
type invocation struct {
	bin   string
	toks  []string
	where string // human-readable origin, for the failure message
}

// findInvocations pulls every binary reference out of s that is written as a
// command rather than mentioned in a sentence. "dfsctl is the REST client" and
// "run `dfsctl login`" both contain the binary name; only the second is claiming
// a command exists, and only the second is worth checking. A reference counts
// when it is quoted or backticked, or when it starts its own line — which is how
// Example blocks and copy-pasteable log messages are written.
func findInvocations(s, where string) []invocation {
	var out []invocation
	for bin := range roots() {
		for i := 0; i+len(bin)+1 < len(s); i++ {
			if !strings.HasPrefix(s[i:], bin+" ") || !isCommandStart(s, i) {
				continue
			}
			// "dfsctl is the REST client" opens the root's own description at
			// offset 0, which looks exactly like a command line. No command is
			// named after an English function word, so one in the first slot means
			// this is a sentence.
			if prosePrefix(s[i+len(bin)+1:]) {
				continue
			}
			rest := s[i+len(bin)+1:]
			if cut := strings.IndexAny(rest, "\n|&;`'\""); cut >= 0 {
				rest = rest[:cut]
			}
			var toks []string
			for _, tok := range strings.Fields(rest) {
				toks = append(toks, strings.TrimRight(tok, ".,:;)"))
			}
			if len(toks) > 0 {
				out = append(out, invocation{bin: bin, toks: toks, where: where})
			}
		}
	}
	return out
}

// proseWords are words that can follow the binary name in a sentence but can
// never be a subcommand.
var proseWords = map[string]bool{
	"is": true, "are": true, "the": true, "a": true, "an": true, "to": true,
	"and": true, "or": true, "will": true, "can": true, "does": true, "has": true,
	"was": true, "from": true, "with": true, "for": true, "in": true, "on": true,
	"talks": true, "uses": true, "needs": true, "requires": true, "commands": true,
	"subcommand": true, "build": true, "instance": true, "itself": true,
	"still": true, "did": true, "survived": true, "exited": true, "process": true,
}

// prosePrefix reports whether the text following a binary name reads as a
// sentence rather than a command line.
func prosePrefix(rest string) bool {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return true
	}
	return proseWords[strings.ToLower(strings.TrimRight(fields[0], ".,:;)"))]
}

// isCommandStart reports whether the binary name at s[i:] is being written as a
// command line: immediately after a quote or backtick, or at the start of a line
// with nothing before it but indentation, a shell prompt, or a relative path.
func isCommandStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case '`', '\'', '"':
		return true
	}
	lineStart := strings.LastIndexByte(s[:i], '\n') + 1
	prefix := strings.TrimLeft(s[lineStart:i], " \t")
	prefix = strings.TrimPrefix(prefix, "$ ")
	prefix = strings.TrimLeft(prefix, " \t")
	return prefix == "" || prefix == "./" || prefix == "sudo " || prefix == "sudo ./"
}

// checkInvocation walks inv against its command tree and returns one message per
// problem found.
func checkInvocation(inv invocation) []string {
	cmd := roots()[inv.bin]
	path := []string{inv.bin}
	var problems []string
	var flags []string

	for _, tok := range inv.toks {
		switch {
		case strings.HasPrefix(tok, "-"):
			flags = append(flags, tok)
			continue
		case placeholderish(tok):
			// An argument, a placeholder or a value — the command path ends here.
			return append(problems, checkFlags(cmd, flags, path, inv.where)...)
		case !cmd.HasSubCommands():
			// A leaf's positional argument.
			return append(problems, checkFlags(cmd, flags, path, inv.where)...)
		}
		child := findChild(cmd, tok)
		if child == nil {
			problems = append(problems, inv.where+": `"+strings.Join(path, " ")+" "+tok+
				"` — "+strings.Join(path, " ")+" has no subcommand \""+tok+"\" (has: "+childNames(cmd)+")")
			return problems
		}
		cmd = child
		path = append(path, tok)
	}
	return append(problems, checkFlags(cmd, flags, path, inv.where)...)
}

// checkFlags validates the long and short flags named in an invocation against
// the command it resolved to, including flags inherited from its parents.
func checkFlags(cmd *cobra.Command, flags, path []string, where string) []string {
	var problems []string
	for _, raw := range flags {
		name := strings.TrimLeft(raw, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if name == "" || placeholderish(name) {
			continue
		}
		if strings.HasPrefix(raw, "--") {
			if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil &&
				cmd.PersistentFlags().Lookup(name) == nil {
				problems = append(problems, where+": `"+strings.Join(path, " ")+
					"` has no flag --"+name)
			}
			continue
		}
		if len(name) == 1 && cmd.Flags().ShorthandLookup(name) == nil &&
			cmd.InheritedFlags().ShorthandLookup(name) == nil {
			problems = append(problems, where+": `"+strings.Join(path, " ")+
				"` has no flag -"+name)
		}
	}
	return problems
}

func findChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
		for _, alias := range c.Aliases {
			if alias == name {
				return c
			}
		}
	}
	return nil
}

func childNames(cmd *cobra.Command) string {
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// TestHelpTextCommandReferences checks every Short, Long and Example string in
// both command trees. This is the text users read at `--help`, which cli.md is
// generated from but which no generator validates.
func TestHelpTextCommandReferences(t *testing.T) {
	var problems []string
	for bin, root := range roots() {
		walkCommands(root, bin, func(cmd *cobra.Command, path string) {
			for field, text := range map[string]string{
				"Short":   cmd.Short,
				"Long":    cmd.Long,
				"Example": cmd.Example,
			} {
				for _, inv := range findInvocations(text, path+" ("+field+")") {
					problems = append(problems, checkInvocation(inv)...)
				}
			}
		})
	}
	reportProblems(t, problems)
}

// TestSourceStringCommandReferences checks the command references in log lines,
// error messages and any other string literal in the tree. A message that tells
// an operator to run a command that does not exist is the same drift as a stale
// Long description, and it reaches them at the worst possible moment.
func TestSourceStringCommandReferences(t *testing.T) {
	var problems []string
	fset := token.NewFileSet()
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable dir is not this test's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", ".planning", "docs":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil //nolint:nilerr // a file that does not parse fails the build, not this test
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			where := filepath.ToSlash(strings.TrimPrefix(path, "../../")) + ":" +
				strconv.Itoa(fset.Position(lit.Pos()).Line)
			for _, inv := range findInvocations(text, where) {
				problems = append(problems, checkInvocation(inv)...)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
	reportProblems(t, problems)
}

// TestDocsCommandReferences checks the hand-written guides. cli.md is generated
// and skipped; everything around it is prose an author has to keep in step by
// hand, which is how a whole page came to document a flag layout the CLI had
// stopped using.
func TestDocsCommandReferences(t *testing.T) {
	var problems []string
	err := filepath.WalkDir("../../docs", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil //nolint:nilerr // an unreadable doc is not this test's business
		}
		if filepath.Base(path) == "cli.md" {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil //nolint:nilerr // ditto
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, "../../"))
		for i, line := range strings.Split(string(body), "\n") {
			where := rel + ":" + strconv.Itoa(i+1)
			for _, inv := range findInvocations(line, where) {
				problems = append(problems, checkInvocation(inv)...)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	reportProblems(t, problems)
}

func reportProblems(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Errorf("%d stale command reference(s) — the command moved or the flag was renamed:\n  %s",
		len(problems), strings.Join(problems, "\n  "))
}

// walkCommands visits cmd and every descendant, passing the full command path.
func walkCommands(cmd *cobra.Command, path string, fn func(*cobra.Command, string)) {
	fn(cmd, path)
	for _, c := range cmd.Commands() {
		walkCommands(c, path+" "+c.Name(), fn)
	}
}
