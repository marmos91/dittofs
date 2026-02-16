package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var (
	logsFollow bool
	logsLines  int
	logsSince  string
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail server logs",
	Long: `Display and optionally follow the DittoFS server logs.

This command reads the log file specified in the configuration and displays
the most recent entries. If the server logs to stdout/stderr, this command
will indicate that logs are not available in a file.

Examples:
  # Show last 100 lines (default)
  dittofs logs

  # Show last 50 lines
  dittofs logs -n 50

  # Follow logs in real-time
  dittofs logs -f

  # Show logs since a specific time
  dittofs logs --since "2024-01-15T10:00:00Z"

  # Combine options
  dittofs logs -f -n 20`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 100, "Number of lines to show")
	logsCmd.Flags().StringVar(&logsSince, "since", "", "Show logs since timestamp (RFC3339 format)")
}

func runLogs(cmd *cobra.Command, args []string) error {
	// Load configuration to find log file
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	logOutput := cfg.Logging.Output

	// Check if logging to file
	if logOutput == "stdout" || logOutput == "stderr" {
		return fmt.Errorf("server is configured to log to %s, not a file\nConfigure 'logging.output' in config to a file path to use this command", logOutput)
	}

	// Check if log file exists
	if _, err := os.Stat(logOutput); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s\nThe server may not have started yet or is logging elsewhere", logOutput)
	}

	// Parse --since time if provided
	var sinceTime time.Time
	if logsSince != "" {
		sinceTime, err = time.Parse(time.RFC3339, logsSince)
		if err != nil {
			return fmt.Errorf("invalid --since format (use RFC3339): %w", err)
		}
	}

	if logsFollow {
		return followLogs(logOutput, logsLines, sinceTime)
	}

	return showLogs(logOutput, logsLines, sinceTime)
}

// showLogs displays the last N lines from the log file.
func showLogs(logFile string, lines int, since time.Time) error {
	file, err := os.Open(logFile)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Read all lines (for simplicity; could optimize for large files)
	var allLines []string
	scanner := bufio.NewScanner(file)
	// Increase buffer size for long log lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !since.IsZero() {
			// Try to parse timestamp from line (assumes RFC3339 at start or in JSON)
			if lineTime := extractTimestamp(line); !lineTime.IsZero() {
				if lineTime.Before(since) {
					continue
				}
			}
		}
		allLines = append(allLines, line)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading log file: %w", err)
	}

	// Show last N lines
	start := 0
	if len(allLines) > lines {
		start = len(allLines) - lines
	}

	for _, line := range allLines[start:] {
		fmt.Println(line)
	}

	return nil
}

// followLogs tails the log file and follows new entries.
func followLogs(logFile string, initialLines int, since time.Time) error {
	// Show initial lines first
	if err := showLogs(logFile, initialLines, since); err != nil {
		return err
	}

	// Set up file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(logFile); err != nil {
		return fmt.Errorf("failed to watch log file: %w", err)
	}

	// Open file for reading new content
	file, err := os.Open(logFile)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Seek to end
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("failed to seek to end of log file: %w", err)
	}

	reader := bufio.NewReader(file)

	// Set up signal handling for graceful exit
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "Following %s (Ctrl+C to stop)...\n", logFile)

	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if event.Op&fsnotify.Write == fsnotify.Write {
				// Read and print new lines
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						break
					}
					fmt.Print(line)
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("watcher error: %w", err)
		}
	}
}

// extractTimestamp attempts to extract a timestamp from a log line.
// Supports common formats: RFC3339 at start, or JSON "time" field.
func extractTimestamp(line string) time.Time {
	// Try RFC3339 at the start of the line
	if len(line) >= 20 {
		if t, err := time.Parse(time.RFC3339, line[:20]); err == nil {
			return t
		}
		// Try with timezone suffix (longer)
		if len(line) >= 25 {
			if t, err := time.Parse(time.RFC3339, line[:25]); err == nil {
				return t
			}
		}
	}

	// Try to find JSON "time" field (simple parsing)
	// Format: {"time":"2024-01-15T10:30:45.123Z",...}
	const timeKey = `"time":"`
	if idx := strings.Index(line, timeKey); idx >= 0 {
		start := idx + len(timeKey)
		end := start + 24 // RFC3339 with milliseconds
		if end <= len(line) {
			// Find the closing quote
			for i := start; i < len(line) && i < start+30; i++ {
				if line[i] == '"' {
					if t, err := time.Parse(time.RFC3339Nano, line[start:i]); err == nil {
						return t
					}
					break
				}
			}
		}
	}

	return time.Time{}
}
