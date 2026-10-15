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

// dtcTools is where the dtc image keeps its copies of the test tools, each
// laid out as nix builds it, with its programs in bin. A variable, so the
// tests can point it elsewhere.
var dtcTools = "/opt"

// layOutTools lays out {tools} at dir from the test tools at the revisions
// flake.lock pins, pjdfstest and pynfs, each of which a suite names by its
// path there. {tools}/pynfs links to pynfs as nix builds it. pjdfstest's
// tests are copied as directories of links to its files, with a link to its
// binary beside them: misc.sh looks for the binary in the directories above a
// test, so it finds that link. Each cell lays it out afresh, over an earlier
// cell's.
func layOutTools(ctx context.Context, root, dir string, w io.Writer) error {
	pjdfstest, err := toolOut(ctx, root, "pjdfstest", w)
	if err != nil {
		return err
	}
	pynfs, err := toolOut(ctx, root, "pynfs", w)
	if err != nil {
		return err
	}
	binary := filepath.Join(pjdfstest, "bin", "pjdfstest")
	if _, err := os.Stat(binary); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := linkTree(filepath.Join(pjdfstest, "share", "pjdfstest", "tests"), filepath.Join(dir, "pjdfstest", "tests")); err != nil {
		return err
	}
	if err := os.Symlink(binary, filepath.Join(dir, "pjdfstest", "pjdfstest")); err != nil {
		return err
	}
	return os.Symlink(pynfs, filepath.Join(dir, "pynfs"))
}

// toolOut returns the directory a tool is laid out in: the dtc image's copy,
// or what nix builds of the flake's package, which the nix store already
// holds after the first build.
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

// linkTree makes dst a copy of the directories under src with a link to each
// of their files, as cp -rs does.
func linkTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return os.Symlink(path, filepath.Join(dst, rel))
	})
}
