package main

import (
	"maps"
	"slices"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// decision: dt doctor checks only the needs the registry names, with the
// checks dt run makes, so the two never disagree about what can run here.
// Tool versions and the like aren't reported. When a suite fails for want of
// something no need names, add that need rather than a check here.
func newDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Which needs this machine meets, and how to meet the rest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := load()
			if err != nil {
				return err
			}
			root, _ := repoRoot()
			checked := checkNeeds(root, r)
			rows := [][]string{{"NEED", "MET", "WHY", "HOW"}}
			for _, n := range slices.Sorted(maps.Keys(checked)) {
				if err := checked[n]; err != nil {
					rows = append(rows, []string{n, "no", err.Error(), needs.Hint(n)})
				} else {
					rows = append(rows, []string{n, "yes", "", ""})
				}
			}
			return table(cmd.OutOrStdout(), rows)
		},
	}
}

// checkNeeds checks every need the registry names, per-test needs included,
// once each, since some probes take seconds. It maps each need to why this
// machine doesn't meet it, or to nil.
func checkNeeds(root string, r *registry.Registry) map[string]error {
	checked := map[string]error{}
	check := func(list []string) {
		for _, n := range list {
			if _, ok := checked[n]; !ok {
				checked[n] = needs.Check(root, n)
			}
		}
	}
	for _, s := range r.Suites {
		for _, u := range r.Units(s) {
			check(u.Needs)
			for _, list := range u.NeedsByTest {
				check(list)
			}
		}
	}
	return checked
}
