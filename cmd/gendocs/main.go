// Command gendocs regenerates docs/CLI.md from the dfs and dfsctl Cobra
// command trees, so the CLI reference never drifts from the actual commands.
//
// Usage:
//
//	go run ./cmd/gendocs            # writes docs/CLI.md
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
	out := flag.String("out", "docs/CLI.md", "output file path")
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

// writeTree renders the command hierarchy as an indented code block.
func writeTree(buf *bytes.Buffer, root *cobra.Command) {
	buf.WriteString("```\n")
	var walk func(c *cobra.Command, depth int)
	walk = func(c *cobra.Command, depth int) {
		if depth == 0 {
			fmt.Fprintf(buf, "%s\n", c.Name())
		} else {
			fmt.Fprintf(buf, "%s%-14s %s\n", strings.Repeat("  ", depth), c.Name(), c.Short)
		}
		kids := visibleSubcommands(c)
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name() < kids[j].Name() })
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(root, 0)
	buf.WriteString("```\n")
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
	if c.Long != "" && strings.TrimSpace(c.Long) != strings.TrimSpace(c.Short) {
		fmt.Fprintf(buf, "%s\n\n", c.Long)
	}

	if c.Runnable() {
		fmt.Fprintf(buf, "```\n%s\n```\n\n", c.UseLine())
	}

	if c.HasExample() {
		fmt.Fprintf(buf, "Examples:\n\n```\n%s\n```\n\n", strings.TrimSpace(c.Example))
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
