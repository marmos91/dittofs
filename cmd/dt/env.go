package main

import (
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// envCommands are the environment commands. They start and stop containers, so
// they stay shell: dt hands each, with its arguments, to test/harness/env.sh.
var envCommands = []struct{ name, short string }{
	{"services", "Start, stop or show the services the suites use: localstack, postgres, kmip"},
	{"stack", "Start, stop, show or mount the compose stack: a server with its S3 on Localstack"},
	{"cleanup", "Stop everything the harness can leave running"},
	{"hooks", "Run git's hooks through the harness, on this machine or in the dtc container"},
}

func newEnv() []*cobra.Command {
	var cmds []*cobra.Command
	for _, c := range envCommands {
		name := c.name
		cmds = append(cmds, &cobra.Command{
			Use:                name + " [ARGS]",
			Short:              c.short,
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return envShell(cmd, name, args)
			},
		})
	}
	return cmds
}

// envShell runs test/harness/env.sh NAME ARGS with dt's input and output, and
// returns its exit status.
func envShell(cmd *cobra.Command, name string, args []string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	script := filepath.Join(root, "test", "harness", "env.sh")
	sh := exec.Command("bash", append([]string{script, name}, args...)...)
	sh.Stdin, sh.Stdout, sh.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	err = sh.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exitStatus(exit.ExitCode())
	}
	return err
}
