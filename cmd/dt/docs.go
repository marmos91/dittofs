package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// The documentation's suite table sits between these markers in suitesDoc,
// relative to the repository root. Its links are relative to that doc.
const (
	suitesDoc   = "docs/internals/testing.md"
	suitesBegin = "<!-- conformance-suites:begin -->"
	suitesEnd   = "<!-- conformance-suites:end -->"
)

func newDocs() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "docs [--check]",
		Short: "Write the documentation's suite tables from the registry, or check them; exit 1 when they differ",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			path := filepath.Join(root, suitesDoc)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			doc := string(data)
			before, rest, begun := strings.Cut(doc, suitesBegin)
			_, after, ended := strings.Cut(rest, suitesEnd)
			if !begun || !ended {
				return fmt.Errorf("%s: no %s followed by %s", suitesDoc, suitesBegin, suitesEnd)
			}
			updated := before + suitesTable(r) + after
			switch {
			case updated == doc:
				return nil
			case check:
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: the suite table differs from %s; run test/harness/bin/dt docs and commit the result\n", suitesDoc, registry.Path)
				return exitStatus(1)
			}
			return os.WriteFile(path, []byte(updated), 0o644)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only check the tables, and exit 1 when they differ")
	return cmd
}

// suitesTable renders, markers included, a row for each suite graded against
// a known file and for each of its options, in the registry's order.
//
// decision: those suites are today's conformance suites, cthon04 and nfstest
// included, which the conformance group leaves out. Pick the rows by group
// once a suite of another kind gets a known file.
func suitesTable(r *registry.Registry) string {
	var b strings.Builder
	b.WriteString(suitesBegin + "\n<!-- Generated from " + registry.Path + " by test/harness/bin/dt docs. Do not edit by hand. -->\n\n")
	b.WriteString("| Suite | Protocol | Profiles | Variants | Tier | Known failures and skips |\n|---|---|---|---|---|---|\n")
	for _, s := range r.Suites {
		for _, u := range r.Units(s) {
			if u.Known == "" {
				continue
			}
			var variants []string
			for _, a := range u.Matrix {
				if a.Name != "profile" {
					variants = append(variants, a.Name+": "+code(a.Values...))
				}
			}
			_, _ = fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | [`%s`](../../%[6]s) |\n",
				u.Name, protocols(u.Fixture), code(profiles(u)...), none(strings.Join(variants, "; ")), code(u.Tier), u.Known)
		}
	}
	b.WriteString("\nFrom [`" + registry.Path + "`](../../" + registry.Path + "): its suites graded against a known file, and their options.\n")
	b.WriteString("Each runs in its tier and the wider ones, `pr` then `merge` then `nightly`; a `manual` one only when asked.\n")
	b.WriteString(suitesEnd)
	return b.String()
}

// protocols names what the fixture serves.
func protocols(f *registry.Fixture) string {
	var p []string
	if f != nil && f.NFS.Serves() {
		p = append(p, "NFS")
	}
	if f != nil && f.SMB.Enabled {
		p = append(p, "SMB")
	}
	return none(strings.Join(p, ", "))
}

// profiles returns the unit's profile axis, or its fixture's one profile.
func profiles(u *registry.Suite) []string {
	for _, a := range u.Matrix {
		if a.Name == "profile" {
			return a.Values
		}
	}
	if u.Fixture != nil && u.Fixture.Profile != "" {
		return []string{u.Fixture.Profile}
	}
	return nil
}

// code joins values, each as code, and says none for no value.
func code(values ...string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "`" + v + "`"
	}
	return none(strings.Join(quoted, ", "))
}

func none(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
