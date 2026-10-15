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
// flake.lock pins, pjdfstest and pynfs, and returns their bin directories,
// which go first on the commands' PATH. pjdfstest's tests are copied as
// directories of links to its files, with a link to its binary beside them:
// misc.sh looks for the binary in the directories above a test, so it finds
// that link. Each cell lays it out afresh, over an earlier cell's.
func layOutTools(ctx context.Context, root, dir string, w io.Writer) ([]string, error) {
	pjdfstest, err := toolOut(ctx, root, "pjdfstest", w)
	if err != nil {
		return nil, err
	}
	pynfs, err := toolOut(ctx, root, "pynfs", w)
	if err != nil {
		return nil, err
	}
	binary := filepath.Join(pjdfstest, "bin", "pjdfstest")
	if _, err := os.Stat(binary); err != nil {
		return nil, err
	}
	at := filepath.Join(dir, "pjdfstest")
	if err := os.RemoveAll(at); err != nil {
		return nil, err
	}
	if err := linkTree(filepath.Join(pjdfstest, "share", "pjdfstest", "tests"), filepath.Join(at, "tests")); err != nil {
		return nil, err
	}
	if err := os.Symlink(binary, filepath.Join(at, "pjdfstest")); err != nil {
		return nil, err
	}
	return []string{filepath.Join(pjdfstest, "bin"), filepath.Join(pynfs, "bin")}, nil
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

// prependPath puts dirs first on the PATH env gives a command.
func prependPath(env, dirs []string) []string {
	return append(env, "PATH="+strings.Join(append(dirs, filepath.SplitList(envPath(env))...), string(os.PathListSeparator)))
}

// envPath is the PATH env gives a command: the last one it sets.
func envPath(env []string) string {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	return path
}

// program finds a command's program on the PATH the command gets, as its
// shell would, where exec looks on dt's own: the tools' bin directories head
// the first. A name with a directory, or one no absolute directory there
// holds, is left for exec to find, as is every name when the two PATHs are
// the same.
func program(name string, env []string) string {
	path := envPath(env)
	if path == os.Getenv("PATH") || strings.ContainsRune(name, '/') {
		return name
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && filepath.IsAbs(dir) && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}
