package main

import (
	"maps"
	"slices"
	"sync"

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
			var all []*registry.Suite
			for _, s := range r.Suites {
				all = append(all, r.Units(s)...)
			}
			checked := checkNeeds(root, all)
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

// checkNeeds checks the needs the given suites name, per-test needs
// included, each once and all at the same time, since a probe can take
// seconds: up to 30 for a stalled Docker daemon. It maps each need to why this
// machine doesn't meet it, or to nil.
func checkNeeds(root string, suites []*registry.Suite) map[string]error {
	names := map[string]bool{}
	for _, u := range suites {
		for _, n := range u.Needs {
			names[n] = true
		}
		for _, list := range u.NeedsByTest {
			for _, n := range list {
				names[n] = true
			}
		}
	}
	checked := make(map[string]error, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for n := range names {
		wg.Go(func() {
			err := needs.Check(root, n)
			mu.Lock()
			checked[n] = err
			mu.Unlock()
		})
	}
	wg.Wait()
	return checked
}
