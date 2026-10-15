package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// dtcTools is where the dtc image keeps its copies of the test tools: a
// directory per tool with its programs in bin, as in nix's output, and for
// pjdfstest the share/pjdfstest nix's output has too. A variable, so the tests
// can point it elsewhere.
var dtcTools = "/opt"

// toolDirs maps each test tool to the directory of its output {tools}/NAME
// links to: pjdfstest's share/pjdfstest holds its binary beside its tests,
// where misc.sh looks for it, and pynfs's whole output holds bin/pynfs-4.x.
var toolDirs = map[string]string{"pjdfstest": "share/pjdfstest", "pynfs": "."}

// layOutTools makes {tools} at dir, with a link {tools}/NAME for each tool in
// names, to the tool at the revision flake.lock pins. Each link is made once
// per run: one already there, from an earlier cell, is kept.
func layOutTools(ctx context.Context, root, dir string, names []string, w io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		sub, ok := toolDirs[name]
		if !ok {
			return fmt.Errorf("dt has no tool %q", name)
		}
		link := filepath.Join(dir, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		out, err := toolOut(ctx, root, name, w)
		if err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(out, sub), link); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

// toolOut returns where a tool is, with its programs in bin: the dtc image's
// copy, or what nix builds of the flake's package, which the nix store
// already holds after the first build.
func toolOut(ctx context.Context, root, name string, w io.Writer) (string, error) {
	out := filepath.Join(dtcTools, name)
	if os.Getenv("DT_IN_CONTAINER") != "1" {
		argv := []string{"nix", "build", ".#" + name, "--no-link", "--print-out-paths"}
		shown := strings.Join(argv, " ")
		_, _ = fmt.Fprintf(w, "[dt] $ %s\n", shown)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir, cmd.Stderr = root, w
		printed, err := cmd.Output()
		switch code, err := exitCode(err); {
		case errors.Is(err, exec.ErrNotFound):
			return "", errors.New("nix isn't installed: install it, or run dt in dtc, whose image has the tools")
		case err != nil:
			return "", err
		case code != 0:
			return "", fmt.Errorf("`%s` exited %d", shown, code)
		}
		out = strings.TrimSpace(string(printed))
	}
	if _, err := os.Stat(filepath.Join(out, "bin")); err != nil {
		return "", err
	}
	return out, nil
}
