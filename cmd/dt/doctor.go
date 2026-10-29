package main

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/needs"
)

// decision: dt doctor reports only the needs the registry names, checked as
// dt run checks them, and then fails on each expired quarantine entry. What
// dt itself can't do yet isn't a need of the machine, and dt list shows which
// suites it stops. Tool versions and the like aren't reported: when a suite
// fails for want of something no need names, add that need rather than a
// check here.
func newDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Which needs this machine meets, and how to meet the rest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			var lists [][]string
			for _, s := range r.Suites {
				for _, u := range r.Units(s) {
					lists = append(append(lists, u.Needs), slices.Collect(maps.Values(u.NeedsByTest))...)
				}
			}
			checked := checkNeeds(root, lists)
			rows := [][]string{{"NEED", "MET", "WHY", "HOW"}}
			for _, n := range slices.Sorted(maps.Keys(checked)) {
				if err := checked[n]; err != nil {
					rows = append(rows, []string{n, "no", err.Error(), needs.Hint(n)})
				} else {
					rows = append(rows, []string{n, "yes", "", ""})
				}
			}
			if err := table(cmd.OutOrStdout(), rows); err != nil {
				return err
			}
			if len(r.Expired) > 0 {
				return errors.New(strings.Join(r.Expired, "\n"))
			}
			return nil
		},
	}
}

// checkNeeds checks the needs of lists, each once and all at the same time,
// since a probe can take seconds: up to 30 for a stalled Docker daemon. It
// maps each need to why this machine doesn't meet it, or to nil.
func checkNeeds(root string, lists [][]string) map[string]error {
	names := map[string]bool{}
	for _, list := range lists {
		for _, n := range list {
			names[n] = true
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
