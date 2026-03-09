package bench

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/bench"
	"github.com/spf13/cobra"
)

var (
	runThreads        int
	runFileSize       string
	runBlockSize      string
	runDuration       string
	runWorkloads      string
	runSystem         string
	runSave           string
	runMetaFiles      int
	runSmallFileCount int
)

var runCmd = &cobra.Command{
	Use:   "run PATH",
	Short: "Run filesystem benchmarks",
	Long: `Run I/O and metadata benchmarks against the given directory.

No API authentication is required — this operates purely on the filesystem.

Examples:
  # Run all benchmarks with defaults
  dfsctl bench run /mnt/bench

  # Run specific workloads with custom parameters
  dfsctl bench run /mnt/bench --workload seq-write,seq-read --threads 8

  # Save results for later comparison
  dfsctl bench run /mnt/bench --system dittofs --save results/dittofs.json`,
	Args: cobra.ExactArgs(1),
	RunE: runBench,
}

func init() {
	runCmd.Flags().IntVar(&runThreads, "threads", 4, "Number of concurrent I/O workers")
	runCmd.Flags().StringVar(&runFileSize, "file-size", "1GiB", "Size of each test file")
	runCmd.Flags().StringVar(&runBlockSize, "block-size", "4KiB", "I/O block size for random workloads")
	runCmd.Flags().StringVar(&runDuration, "duration", "60s", "Time limit for duration-based workloads")
	runCmd.Flags().StringVar(&runWorkloads, "workload", "", "Comma-separated workloads (default: all)")
	runCmd.Flags().StringVar(&runSystem, "system", "", "Label identifying the system under test")
	runCmd.Flags().StringVar(&runSave, "save", "", "Save results to JSON file")
	runCmd.Flags().IntVar(&runMetaFiles, "meta-files", 1000, "Number of files for metadata workload")
	runCmd.Flags().IntVar(&runSmallFileCount, "small-file-count", 10000, "Number of files for small-files workload")
}

func runBench(cmd *cobra.Command, args []string) error {
	fileSize, err := bench.ParseSize(runFileSize)
	if err != nil {
		return fmt.Errorf("invalid --file-size: %w", err)
	}

	blockSize, err := bench.ParseSize(runBlockSize)
	if err != nil {
		return fmt.Errorf("invalid --block-size: %w", err)
	}

	dur, err := parseGoDuration(runDuration)
	if err != nil {
		return fmt.Errorf("invalid --duration: %w", err)
	}

	cfg := bench.Config{
		Path:           args[0],
		Threads:        runThreads,
		FileSize:       fileSize,
		BlockSize:      blockSize,
		Duration:       dur,
		MetaFiles:      runMetaFiles,
		SmallFileCount: runSmallFileCount,
		System:         runSystem,
	}

	if runWorkloads != "" {
		for _, w := range cmdutil.ParseCommaSeparatedList(runWorkloads) {
			cfg.Workloads = append(cfg.Workloads, bench.WorkloadType(w))
		}
	}

	runner := bench.NewRunner(cfg, func(w bench.WorkloadType, pct float64) {
		fmt.Fprintf(os.Stderr, "\r  %s: %.0f%%", w, pct*100)
	})

	if err := runner.Validate(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Benchmarking %s (%d threads, %s files, %s duration)\n",
		cfg.Path, cfg.Threads, bench.FormatSize(cfg.FileSize), cfg.Duration)

	result, err := runner.Run(cmd.Context())
	if err != nil {
		return fmt.Errorf("benchmark failed: %w", err)
	}

	fmt.Fprintln(os.Stderr) // Clear progress line

	// Save JSON if requested.
	if runSave != "" {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal results: %w", err)
		}
		if err := os.WriteFile(runSave, data, 0o644); err != nil {
			return fmt.Errorf("save results: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Results saved to %s\n", runSave)
	}

	// Print table/json/yaml output.
	return cmdutil.PrintResource(os.Stdout, result, ResultTable{Result: result})
}
