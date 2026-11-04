package prof

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/pprof"
)

// profiles maps each file the watch writes on failure to the runtime profile
// it holds and the pprof debug level it is written at: the goroutine dump as
// text a person reads first, the heap and allocation profiles as compressed
// protobuf for go tool pprof.
var profiles = []struct {
	file  string
	name  string
	debug int
}{
	{"goroutines.txt", "goroutine", 2},
	{"heap.pb.gz", "heap", 0},
	{"allocs.pb.gz", "allocs", 0},
}

// writeProfiles writes every profile into dir, carrying on past a file it
// cannot write so the others still land.
func writeProfiles(dir string) error {
	var errs []error
	for _, p := range profiles {
		errs = append(errs, writeProfile(filepath.Join(dir, p.file), p.name, p.debug))
	}

	return errors.Join(errs...)
}

func writeProfile(path, name string, debug int) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	return pprof.Lookup(name).WriteTo(f, debug)
}
