package orchestrator

import (
	"os"
	"runtime"
	"runtime/debug"
)

// LocalSystem captures the host/runtime environment of the current process.
// Producers call this once and pass the result into Run.
func LocalSystem() System {
	host, _ := os.Hostname()
	return System{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
		GoVersion: runtime.Version(),
		Hostname:  host,
	}
}

// BuildGitSHA returns the VCS revision embedded by the Go toolchain at build
// time (runtime/debug.BuildInfo), or "" when unavailable (e.g. `go run`, or a
// build with -buildvcs=false). Callers may override it with an explicit flag;
// this is the zero-config fallback so a normal `go build` artifact self-stamps
// its commit without a hardcoded time.Now()-style value.
func BuildGitSHA() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
