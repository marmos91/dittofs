package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// parseEnvFile loads simple KEY=VALUE lines from path into the process
// env via os.Setenv. Lines starting with '#' and blank lines are
// ignored. No quote/multiline handling — the harness only consumes
// a handful of S3 vars; keep the parser deliberately minimal.
//
// Existing env vars are NOT overwritten so a CLI-passed
// AWS_S3_BUCKET still wins over a checked-in .env.
func parseEnvFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path is the point
	if err != nil {
		return fmt.Errorf("envfile: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return fmt.Errorf("envfile: %s:%d: missing '='", path, lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if _, ok := os.LookupEnv(key); ok {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf("envfile: setenv %s: %w", key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("envfile: scan %s: %w", path, err)
	}
	return nil
}
