// Command gendocs regenerates docs/guide/cli.md from the dfs and dfsctl Cobra
// command trees, so the CLI reference never drifts from the actual commands.
//
// Usage:
//
//	go run ./cmd/gendocs            # writes docs/guide/cli.md
//	go run ./cmd/gendocs -out FILE  # writes to FILE
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	dfs "github.com/marmos91/dittofs/cmd/dfs/commands"
	dfsctl "github.com/marmos91/dittofs/cmd/dfsctl/commands"
)

func main() {
	out := flag.String("out", "docs/guide/cli.md", "output file path")
	flag.Parse()

	var buf bytes.Buffer

	buf.WriteString("# CLI Reference\n\n")
	buf.WriteString("DittoFS ships two binaries:\n\n")
	buf.WriteString("- **`dfs`** — the server daemon. Runs the protocol adapters and the control-plane API; manages the local config file and the server process.\n")
	buf.WriteString("- **`dfsctl`** — the REST client. Talks to a running `dfs` over its control-plane API to manage users, groups, shares, stores, and adapters.\n\n")
	buf.WriteString("This page is generated from the command definitions (`go run ./cmd/gendocs`). Do not edit it by hand. Run `dfs <command> --help` or `dfsctl <command> --help` for the same content at the terminal.\n\n")

	for _, bin := range []struct {
		root *cobra.Command
		name string
	}{
		{dfs.GetRootCmd(), "dfs"},
		{dfsctl.GetRootCmd(), "dfsctl"},
	} {
		bin.root.DisableAutoGenTag = true
		fmt.Fprintf(&buf, "## `%s`\n\n", bin.name)
		writeTree(&buf, bin.root)
		buf.WriteString("\n")
		writeCommand(&buf, bin.root)
	}

	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}

// writeTree renders the command hierarchy as a nested, clickable index. Each
// entry links to that command's own section, whose header is the command path
// in a level-3 heading, so readers can jump straight to any command on the page.
func writeTree(buf *bytes.Buffer, root *cobra.Command) {
	var walk func(c *cobra.Command, depth int)
	walk = func(c *cobra.Command, depth int) {
		indent := strings.Repeat("  ", depth)
		if c.Short != "" {
			fmt.Fprintf(buf, "%s- [`%s`](#%s) — %s\n", indent, c.CommandPath(), anchor(c.CommandPath()), c.Short)
		} else {
			fmt.Fprintf(buf, "%s- [`%s`](#%s)\n", indent, c.CommandPath(), anchor(c.CommandPath()))
		}
		kids := visibleSubcommands(c)
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name() < kids[j].Name() })
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(root, 0)
	buf.WriteString("\n")
}

// splitExamples separates a leading description from a trailing "Examples:" block
// embedded in a Cobra Long string, so each can be rendered appropriately.
func splitExamples(long string) (desc, examples string) {
	lines := strings.Split(long, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "Examples:" {
			// Trim only surrounding blank lines, preserving each example line's
			// indentation so dedent can strip the common indent uniformly.
			return strings.TrimRight(strings.Join(lines[:i], "\n"), "\n"),
				strings.Trim(strings.Join(lines[i+1:], "\n"), "\n")
		}
	}
	return long, ""
}

// dedent strips the common leading-space indent from a block (Cobra example
// blocks are conventionally indented two spaces).
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	min := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " "))
		if min == -1 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return s
	}
	for i, ln := range lines {
		if len(ln) >= min {
			lines[i] = ln[min:]
		}
	}
	return strings.Join(lines, "\n")
}

// renderDesc emits a command description, fencing any indented, pre-formatted
// run (aligned subcommand tables, option lists, numbered steps) that Cobra Long
// strings conventionally use. Without fencing, Markdown/MDX collapses the
// leading whitespace and runs every line together. Non-indented prose is left
// as prose so it wraps and renders normally.
func renderDesc(buf *bytes.Buffer, desc string) {
	indented := func(s string) bool {
		return strings.HasPrefix(s, "  ") || strings.HasPrefix(s, "\t")
	}
	lines := strings.Split(desc, "\n")
	for i := 0; i < len(lines); {
		if indented(lines[i]) {
			// Extend the run over indented lines and any interior blank lines,
			// trimming trailing blanks so they don't pad the fence.
			j, last := i, i
			for j < len(lines) && (indented(lines[j]) || strings.TrimSpace(lines[j]) == "") {
				if indented(lines[j]) {
					last = j
				}
				j++
			}
			fmt.Fprintf(buf, "```\n%s\n```\n\n", dedent(strings.Join(lines[i:last+1], "\n")))
			i = last + 1
			continue
		}
		// Gather a prose run up to the next indented line.
		j := i
		for j < len(lines) && !indented(lines[j]) {
			j++
		}
		if prose := strings.TrimSpace(strings.Join(lines[i:j], "\n")); prose != "" {
			fmt.Fprintf(buf, "%s\n\n", prose)
		}
		i = j
	}
}

// anchor slugifies a command path the way GitHub-flavored Markdown anchors
// section headers: lowercase, backticks dropped, spaces to hyphens.
func anchor(path string) string {
	s := strings.ToLower(path)
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// writeCommand emits a section per command with usage, description, and flags,
// recursing into subcommands.
func writeCommand(buf *bytes.Buffer, c *cobra.Command) {
	if !c.IsAvailableCommand() && !c.IsAdditionalHelpTopicCommand() && c.HasParent() {
		return
	}

	fmt.Fprintf(buf, "### `%s`\n\n", c.CommandPath())
	if c.Short != "" {
		fmt.Fprintf(buf, "%s\n\n", c.Short)
	}
	// Many commands embed an "Examples:" block inside Long as plain text. Split
	// it out so the prose renders as prose and the commands render as a fenced,
	// highlighted code block (instead of headings + smart-quoted dashes).
	desc, embeddedExamples := splitExamples(c.Long)
	if d := strings.TrimSpace(desc); d != "" && d != strings.TrimSpace(c.Short) {
		renderDesc(buf, desc)
	}

	if c.Runnable() {
		fmt.Fprintf(buf, "```\n%s\n```\n\n", c.UseLine())
	}

	examples := strings.TrimSpace(c.Example)
	if examples == "" {
		examples = embeddedExamples
	}
	if examples != "" {
		fmt.Fprintf(buf, "**Examples:**\n\n```bash\n%s\n```\n\n", dedent(examples))
	}

	writeFlags(buf, "Flags", c.NonInheritedFlags())
	writeFlags(buf, "Global flags", c.InheritedFlags())

	kids := visibleSubcommands(c)
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name() < kids[j].Name() })
	for _, k := range kids {
		writeCommand(buf, k)
	}
}

func writeFlags(buf *bytes.Buffer, title string, fs interface{ FlagUsages() string }) {
	usages := strings.TrimRight(fs.FlagUsages(), "\n")
	if usages == "" {
		return
	}
	fmt.Fprintf(buf, "%s:\n\n```\n%s\n```\n\n", title, usages)
}

func visibleSubcommands(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, k := range c.Commands() {
		// Skip hidden commands and the auto-generated "help" pseudo-command
		// that Cobra attaches to every command. Everything else — including
		// "completion" — is a real, documented command.
		if k.Hidden || k.Name() == "help" {
			continue
		}
		out = append(out, k)
	}
	return out
}
