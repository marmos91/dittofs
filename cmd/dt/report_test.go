package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

// TestMain runs this test binary as dt when a test starts it so, to stop a
// real dt process.
func TestMain(m *testing.M) {
	if os.Getenv("DT_TEST_AS_DT") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// dt report shows the latest run by default, any other by its name or its
// path, as text, md or json, and with --failures only the new failures. A
// file among the runs is no run. With no run at all it says so and succeeds,
// in json on stderr; a run that isn't there and a format dt doesn't write are
// errors.
func TestReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the suites run true and false")
	}
	checkout(t)
	results := filepath.Join(t.TempDir(), "results")
	for _, format := range report.Formats {
		var out, errOut bytes.Buffer
		root := newRoot(&out)
		root.SetErr(&errOut)
		root.SetArgs([]string{"report", "--format", format, "--results", results})
		err := root.Execute()
		got, other := out.String(), errOut.String()
		if format == "json" {
			got, other = other, got
		}
		if err != nil || got != "no run in "+results+"\n" || other != "" {
			t.Errorf("%s with no run: %v, out %q, stderr %q", format, err, out.String(), errOut.String())
		}
	}
	_, _ = dt(t, "run", "s", "--results", results)
	_, _ = dt(t, "run", "fails", "--results", results)
	runs, _ := os.ReadDir(results)
	if len(runs) != 2 {
		t.Fatalf("runs %v", runs)
	}
	first := runs[0].Name()
	if err := os.WriteFile(filepath.Join(results, "zz-notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	latest, err := dt(t, "report", "--results", results)
	if err != nil || !strings.Contains(latest, "fails  failed") {
		t.Errorf("latest: %v\n%s", err, latest)
	}
	for _, run := range []string{first, filepath.Join(results, first)} {
		if out, err := dt(t, "report", run, "--results", results); err != nil || !strings.Contains(out, "s     passed") {
			t.Errorf("%s: %v\n%s", run, err, out)
		}
	}
	out, err := dt(t, "report", "--format", "json", "--failures", "--results", results)
	var doc struct {
		Results struct {
			Tests []struct{ Name, Status string }
		}
	}
	if err != nil || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Results.Tests) != 1 || doc.Results.Tests[0].Name != "false" {
		t.Errorf("json --failures: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"report", "nope"}, {"report", "--format", "html"}, {"report", first, first}} {
		if _, err := dt(t, append(args, "--results", results)...); err == nil {
			t.Errorf("dt %s: no error", strings.Join(args, " "))
		}
	}
}

// A run killed mid-way still reports the cells it finished: summary.json is
// rewritten after each cell, so dt report shows the first cell of a run
// killed in its second, report.md shows it too, and the cell has its JUnit
// file.
func TestReportAfterAKilledRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the suites run sh")
	}
	root := t.TempDir()
	y := "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\ngroups: { g: [first, hangs] }\nsuites:\n" +
		"  first: { cmd: \"false\" }\n" +
		"  hangs: { cmd: { sh: \"echo $$ > {out}/pid && exec sleep 60\" } }\n"
	if err := os.MkdirAll(filepath.Join(root, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "test", "suites.yaml"), []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	results := filepath.Join(root, "results")
	log, err := os.Create(filepath.Join(t.TempDir(), "dt.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	proc := exec.Command(os.Args[0], "run", "g", "--results", results)
	proc.Env = append(os.Environ(), "DT_TEST_AS_DT=1")
	proc.Stdout, proc.Stderr = log, log
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	var pid []byte
	for deadline := time.Now().Add(30 * time.Second); len(pid) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = proc.Process.Kill()
			t.Fatal("the second cell never started")
		}
		files, _ := filepath.Glob(filepath.Join(results, "*", "hangs", "pid"))
		if len(files) == 1 {
			pid, _ = os.ReadFile(files[0])
		}
	}
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()
	if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil {
		if p, err := os.FindProcess(n); err == nil {
			_ = p.Kill()
		}
	}

	out, err := dt(t, "report", "--results", results)
	if err != nil || !strings.Contains(out, "first  failed") || strings.Contains(out, "hangs") || !strings.Contains(out, "first  false        `false` exited 1") {
		t.Errorf("dt report: %v\n%s", err, out)
	}
	runs, _ := filepath.Glob(filepath.Join(results, "*"))
	if len(runs) != 1 {
		t.Fatalf("runs %v", runs)
	}
	if md, err := os.ReadFile(filepath.Join(runs[0], "report.md")); err != nil || !strings.Contains(string(md), "| `first` | failed |") || strings.Contains(string(md), "hangs") {
		t.Errorf("report.md: %v\n%s", err, md)
	}
	if _, err := os.Stat(filepath.Join(runs[0], "junit", "first.xml")); err != nil {
		t.Error(err)
	}
}

// dt report --failures --post CELLS RUN... posts the runs it's given together,
// as the cells of one tier run, each of which a CI job wrote, with the token
// from the environment: the develop-status issue it opens lists the new
// failures of both, and a planned cell with no run as not run. --post needs
// --failures, and with no run at all it fails.
func TestReportPost(t *testing.T) {
	checkout(t)
	results := filepath.Join(t.TempDir(), "results")
	var runs []string
	for _, cell := range []string{"pynfs/badger-s3/0", "pynfs/badger-s3/1"} {
		r := report.Run{Name: "20261022T1000" + strconv.Itoa(len(runs)) + "0Z-abcdef12", Commit: "abcdef12", Branch: "develop", Tier: "merge", Cells: []report.Cell{
			{Name: cell, Result: report.Failed, Tests: []report.Test{{Name: "LOCK1", Status: report.Failed, Grade: report.New}}},
		}}
		if err := r.Write(filepath.Join(results, r.Name)); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r.Name)
	}
	var opened string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer tok":
			http.Error(w, "Bad credentials", http.StatusUnauthorized)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, "[]")
		case r.Method == http.MethodPost:
			var in struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			opened = in.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "state": "open", "body": in.Body})
		}
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("GITHUB_TOKEN", "tok")

	planned := "pynfs/badger-s3/0,pynfs/badger-s3/1,pynfs/badger-s3/2"
	if _, err := dt(t, append([]string{"report", "--post", planned, "--results", results}, runs...)...); err == nil || opened != "" {
		t.Fatalf("--post without --failures: %v, opened %q", err, opened)
	}
	if _, err := dt(t, "report", "--failures", "--post", planned, "--results", t.TempDir()); err == nil || opened != "" {
		t.Fatalf("no run: %v, opened %q", err, opened)
	}
	if _, err := dt(t, append([]string{"report", "--failures", "--post", planned, "--results", results}, runs...)...); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| `pynfs/badger-s3/0` | `LOCK1` | new | 1 |", "| `pynfs/badger-s3/1` | `LOCK1` | new | 1 |", "| `pynfs/badger-s3/2` |  | not run | 1 |"} {
		if !strings.Contains(opened, row) {
			t.Errorf("no row %q in\n%s", row, opened)
		}
	}
}
