package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config declares a run. It is the file form of the `run` flags; CLI flags
// override any field set here. Credentials are never read from config — they
// stay in the environment (plan invariant).
type Config struct {
	Systems   []string `yaml:"systems"`   // system labels; local/smoke default to one
	Workloads []string `yaml:"workloads"` // subset of knownWorkloads; empty = all
	Sizes     []string `yaml:"sizes"`     // size classes or explicit fio sizes; empty = medium

	Target  string `yaml:"target"`  // already-mounted path (--local)
	Results string `yaml:"results"` // results dir
	Threads int    `yaml:"threads"` // fio numjobs
	Runtime int    `yaml:"runtime"` // fio runtime seconds
	Engine  string `yaml:"engine"`  // fio ioengine
	FioBin  string `yaml:"fio_bin"` // fio binary path

	Bucket   string `yaml:"bucket"`   // S3 bucket for managed S3-backed backends
	Endpoint string `yaml:"endpoint"` // S3 endpoint URL (creds stay in env)
}

// loadConfig reads a dfsbench YAML config. A missing path is not an error only
// when path is empty (no --config given).
func loadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}
