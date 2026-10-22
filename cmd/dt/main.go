// Command dt runs DittoFS's test suites from one registry, test/suites.yaml.
// Its design is docs/internals/rfcs/rfc-29-test-harness.md.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/run"
)

// exitUsage is the status for a usage or registry error.
const exitUsage = 3

func main() {
	err := newRoot(os.Stdout).Execute()
	var status exitStatus
	switch {
	case errors.As(err, &status):
		os.Exit(int(status))
	case err != nil:
		fmt.Fprintln(os.Stderr, "dt:", err)
		os.Exit(exitUsage)
	}
}

func newRoot(out io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "dt",
		Short:         "Run DittoFS's test suites from test/suites.yaml",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(out)
	root.AddCommand(newList(), newPlan(), newRun(), newReport(), newHistory(), newDoctor(), newKnown(), newDocs(), alias("unit"), alias("integration"), alias("e2e"))
	root.AddCommand(newEnv()...)
	return root
}

func newList() *cobra.Command {
	var tier string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Suites, options and groups, with their tier, format and needs, and whether they can run here",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			if tier != "" && !slices.Contains(registry.Tiers, tier) {
				return fmt.Errorf("tier %q is not one of %s", tier, strings.Join(registry.Tiers, ", "))
			}
			type entry struct {
				Name   string   `json:"name"`
				Tier   string   `json:"tier"`
				Format string   `json:"format"`
				Needs  []string `json:"needs,omitempty"`
				// RunsHere is whether dt run would run the suite's cells here,
				// as dt run decides it: Unsupported names what dt doesn't do
				// yet, and Unmet the needs this machine doesn't meet.
				// UnmetByTest gives, per needs_by_test glob whose tests the
				// tier plans, the extra needs of those tests it doesn't meet.
				RunsHere    bool                `json:"runs_here"`
				Unsupported string              `json:"unsupported,omitempty"`
				Unmet       []string            `json:"unmet,omitempty"`
				UnmetByTest map[string][]string `json:"unmet_by_test,omitempty"`
			}
			var listed []*registry.Suite
			byTest := map[*registry.Suite]map[string][]string{}
			var lists [][]string
			for _, s := range r.Suites {
				for _, u := range r.Units(s) {
					if tier != "" && !plan.RunsIn(u.Tier, tier) {
						continue
					}
					if byTest[u], err = plan.NeedsByTest(root, u, tier); err != nil {
						return err
					}
					listed = append(listed, u)
					lists = append(append(lists, u.Needs), slices.Collect(maps.Values(byTest[u]))...)
				}
			}
			checked := checkNeeds(root, lists)
			check := func(n string) error { return checked[n] }
			var suites []entry
			for _, u := range listed {
				why, unmet := run.Blocked(u, u.Needs, check)
				e := entry{Name: u.Name, Tier: u.Tier, Format: u.Format, Needs: u.Needs,
					RunsHere: why == "", Unmet: unmet, UnmetByTest: map[string][]string{}}
				if why != "" && len(unmet) == 0 {
					e.Unsupported = why
				}
				for glob, list := range byTest[u] {
					if _, missing := run.Blocked(u, list, check); len(missing) > 0 {
						e.UnmetByTest[glob] = missing
					}
				}
				suites = append(suites, e)
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"suites": suites, "groups": r.Groups})
			}
			rows := [][]string{{"SUITE", "TIER", "FORMAT", "NEEDS", "HERE"}}
			for _, e := range suites {
				here := "yes"
				if !e.RunsHere {
					here = "no: " + cmp.Or(e.Unsupported, strings.Join(e.Unmet, " "))
				}
				for _, glob := range slices.Sorted(maps.Keys(e.UnmetByTest)) {
					here += "; not " + glob + ": " + strings.Join(e.UnmetByTest[glob], " ")
				}
				rows = append(rows, []string{e.Name, e.Tier, e.Format, strings.Join(e.Needs, " "), here})
			}
			rows = append(rows, nil, []string{"GROUP", "SUITES"})
			for _, g := range slices.Sorted(maps.Keys(r.Groups)) {
				rows = append(rows, []string{g, strings.Join(r.Groups[g], " ")})
			}
			return table(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "", "only the suites tier T runs")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newPlan() *cobra.Command {
	var tier, base, shardFlag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "The cells a tier runs, each with its place",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			var changed []string
			if base != "" {
				if changed, err = changedFiles(root, base); err != nil {
					return err
				}
			}
			cells, err := plan.Plan(root, r, tier, changed)
			if err != nil {
				return err
			}
			if shardFlag != "" {
				if cells, err = shardCells(cells, shardFlag); err != nil {
					return err
				}
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"tier": tier, "cells": cells})
			}
			rows := [][]string{{"CELL", "PLACE", "BLOCKING", "TIMEOUT", "NOTE"}}
			for _, c := range cells {
				rows = append(rows, []string{c.Name, c.Place, strconv.FormatBool(c.Blocking), strconv.Itoa(c.TimeoutMinutes) + "m", strings.Join(c.Notes, "; ")})
			}
			return table(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "", "the tier to plan: pr, merge, nightly or manual (required)")
	cmd.Flags().StringVar(&base, "base", "", "compare the branch with REF: a docs-only change plans nothing, and suites with paths run only when touched")
	cmd.Flags().StringVar(&shardFlag, "shard", "", "only shard I of N of the cells, as I/N")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	_ = cmd.MarkFlagRequired("tier")
	return cmd
}

// shardCells keeps the I-th of N slices of the cells, dealt in turn.
func shardCells(cells []plan.Cell, spec string) ([]plan.Cell, error) {
	is, ns, ok := strings.Cut(spec, "/")
	i, err1 := strconv.Atoi(is)
	n, err2 := strconv.Atoi(ns)
	if !ok || err1 != nil || err2 != nil || n < 1 || i < 1 || i > n {
		return nil, fmt.Errorf("--shard %q: expected I/N with 1 <= I <= N", spec)
	}
	var out []plan.Cell
	for j := i - 1; j < len(cells); j += n {
		out = append(out, cells[j])
	}
	return out, nil
}

// changedFiles lists the paths the branch changes since it left base.
func changedFiles(root, base string) ([]string, error) {
	// --no-renames lists both sides of a rename, so a file moved out of a
	// suite's paths still touches them.
	out, err := exec.Command("git", "-C", root, "diff", "-z", "--no-renames", "--name-only", base+"...HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	// Never nil: an empty change plans nothing, where nil plans every cell.
	return slices.DeleteFunc(strings.Split(string(out), "\x00"), func(p string) bool { return p == "" }), nil
}

func load() (string, *registry.Registry, error) {
	root, err := repoRoot()
	if err != nil {
		return "", nil, err
	}
	r, err := registry.Load(root)
	return root, r, err
}

// repoRoot finds the checkout by walking up to the directory holding the registry.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, registry.Path)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside a DittoFS checkout: no " + registry.Path + " above the current directory")
		}
		dir = parent
	}
}

// table writes rows aligned in columns. A write error surfaces at Flush.
func table(out io.Writer, rows [][]string) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	return w.Flush()
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
