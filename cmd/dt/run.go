package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/run"
)

// exitStatus is an error that carries a run's exit status to main.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func newRun() *cobra.Command {
	var tier, results, place, in string
	var only, skip []string
	var ci bool
	var retry int
	options := map[string]bool{}
	cmd := &cobra.Command{
		Use:   "run SUITE|GROUP[/CELL] [--OPTION] [TEST...] | --place P",
		Short: "Run suites and write their results and logs; exit 0 when all passed, 1 when one failed, 2 when one didn't run",
		Args: func(cmd *cobra.Command, args []string) error {
			if place != "" {
				var bad []string
				cmd.Flags().Visit(func(f *pflag.Flag) {
					if f.Name == "only" || f.Name == "skip" || options[f.Name] {
						bad = append(bad, "--"+f.Name)
					}
				})
				if len(bad) > 0 {
					return fmt.Errorf("--place runs what dt plan gives the place: it takes no %s", strings.Join(bad, ", "))
				}
				return cobra.NoArgs(cmd, args)
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains([]string{"", "host", "container", "auto"}, in) {
				return fmt.Errorf("--in %q is not one of host, container, auto", in)
			}
			root, r, err := load()
			if err != nil {
				return err
			}
			var cells []run.Cell
			if place != "" {
				cells, err = placeCells(root, r, tier, place)
			} else {
				var option []string
				cmd.Flags().Visit(func(f *pflag.Flag) {
					if options[f.Name] && f.Value.String() == "true" {
						option = append(option, f.Name)
					}
				})
				if len(option) > 1 {
					return fmt.Errorf("one option at a time, not --%s and --%s", option[0], option[1])
				}
				cells, err = run.Select(root, r, args[0], first(option), tier, only, skip, args[1:])
			}
			if err != nil {
				return err
			}
			if inContainer(root, cells, in, ci, place) {
				// dtc sees the checkout alone, so results elsewhere would stay in the container.
				if rel, err := filepath.Rel(root, resultsDir(root, results)); err != nil || !filepath.IsLocal(rel) {
					return fmt.Errorf("--results %s is outside the checkout, the only place dtc writes to", results)
				}
				return dtc(cmd, root, again(cmd, args))
			}
			return runCells(cmd, root, cells, run.Options{Root: root, Results: resultsDir(root, results), CI: ci, Out: cmd.OutOrStdout(), Tier: tier, Retry: retry, Place: place})
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "pr", "run at tier T, or at a suite's own tier when wider: it sets args, env and select")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these suites of the group")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "skip these suites of the group")
	cmd.Flags().BoolVar(&ci, "ci", false, "an unmet need fails the run instead of skipping the cell, and new failures are annotated")
	cmd.Flags().IntVar(&retry, "retry", 0, "run each test that failed new again, up to N times: a pass on a retry is flaky")
	cmd.Flags().StringVar(&results, "results", "", "where results go, relative to the checkout's root (default test/harness/results)")
	cmd.Flags().StringVar(&place, "place", "", "run the cells of the tier that dt plan gives place P, as the dedicated host does; that place's host need is met")
	cmd.Flags().StringVar(&in, "in", "", "where the run happens: host, container (dtc) or auto, which moves it to dtc when a cell needs what only the container gives (default auto, host with --ci)")
	// Each suite's options are flags: dt run smbtorture --kerberos.
	if _, r, err := load(); err == nil {
		for _, s := range r.Suites {
			for _, o := range s.Options {
				if cmd.Flags().Lookup(o.Name) == nil {
					cmd.Flags().Bool(o.Name, false, "run the suite's "+o.Name+" option")
					options[o.Name] = true
				}
			}
		}
	}
	return cmd
}

// need checks a need on this machine; the tests replace it.
var need = needs.Check

// inContainer reports whether the run goes to dtc: with --in container, or
// with --in auto, the default without --ci, when a cell needs what only the
// container gives and Docker, which dtc runs in, is here; Docker is checked
// only then, since its check can take a while. A run inside dtc runs there
// whatever --in says.
func inContainer(root string, cells []run.Cell, in string, ci bool, place string) bool {
	if os.Getenv("DT_IN_CONTAINER") == "1" {
		return false
	}
	if in == "" {
		in = "auto"
		if ci {
			in = "host"
		}
	}
	return in == "container" || in == "auto" && run.Container(cells, func(n string) error {
		if n == "host:"+place {
			return nil
		}
		return need(root, n)
	}) && need(root, "docker") == nil
}

// again is the dt run command line as given, with --in host, for dtc to run.
func again(cmd *cobra.Command, args []string) []string {
	argv := append([]string{"run"}, args...)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "in" {
			return
		}
		v := f.Value.String()
		if s, ok := f.Value.(pflag.SliceValue); ok {
			v = strings.Join(s.GetSlice(), ",")
		}
		argv = append(argv, "--"+f.Name+"="+v)
	})
	return append(argv, "--in=host")
}

// dtc runs dt with argv in the dev container, through test/harness/bin/dtc,
// and gives back its exit status as this run's: dt's 1 to 3 as they are, and
// any other, docker's own or a kill by a signal, as 2, the run didn't run.
// Ctrl-C reaches dtc from the terminal as it reaches dt, which waits for dtc
// to stop; a SIGTERM sent to dt alone is passed on.
func dtc(cmd *cobra.Command, root string, argv []string) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[dt] in dtc: dt %s\n", strings.Join(argv, " "))
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	c := exec.Command(filepath.Join(root, "test", "harness", "bin", "dtc"), argv...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM {
					_ = c.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err := c.Wait()
	close(done)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 1 && code <= 3 {
			return exitStatus(code)
		}
		return exitStatus(2)
	}
	return err
}

// runCells runs the cells, prints their outcomes and where the results went,
// and returns the run's exit status.
func runCells(cmd *cobra.Command, root string, cells []run.Cell, o run.Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := run.Run(ctx, cells, o)
	if rep.Dir == "" {
		return err // the run couldn't start
	}
	rows := [][]string{{"CELL", "RESULT", "TIME", "NOTE"}}
	for _, o := range rep.Outcomes {
		rows = append(rows, []string{o.Name, o.Result, o.Took.String(), o.Note})
	}
	if err := table(cmd.OutOrStdout(), rows); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "results:", rep.Dir)
	if err != nil {
		return err // after the outcomes: summary.json couldn't be written
	}
	if s := rep.Status(); s != 0 {
		return exitStatus(s)
	}
	return nil
}

// placeCells selects the cells of tier that dt plan gives place. An option's
// cells are named SUITE/OPTION/..., which Select takes as the suite, the
// option apart, and the rest.
func placeCells(root string, r *registry.Registry, tier, place string) ([]run.Cell, error) {
	planned, err := plan.Plan(root, r, tier, nil)
	if err != nil {
		return nil, err
	}
	var cells []run.Cell
	for _, c := range planned {
		if c.Place != place {
			continue
		}
		target := c.Name
		if c.Option != "" {
			target = c.Suite + strings.TrimPrefix(c.Name, c.Suite+"/"+c.Option)
		}
		selected, err := run.Select(root, r, target, c.Option, tier, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		for _, s := range selected {
			if s.Name == c.Name {
				cells = append(cells, s)
			}
		}
	}
	if len(cells) == 0 {
		return nil, errors.New("tier " + tier + " plans no cell on place " + place)
	}
	return cells, nil
}

// resultsDir is where runs go: results, relative to the checkout's root, else
// test/harness/results there.
func resultsDir(root, results string) string {
	if results == "" {
		results = filepath.Join("test", "harness", "results")
	}
	if !filepath.IsAbs(results) {
		results = filepath.Join(root, results)
	}
	return results
}

// alias is a command people use today, `dt NAME ARGS`, which is
// `dt run NAME ARGS`.
func alias(name string) *cobra.Command {
	return &cobra.Command{
		Use:                name + " [ARGS...]",
		Short:              "dt run " + name,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			root.SetArgs(append([]string{"run", name}, args...))
			return root.Execute()
		},
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
