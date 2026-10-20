package run

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/known"
	"github.com/marmos91/dittofs/test/harness/report"
)

// The nfstest grader's cases (nfs-mount/nfstest/parse-results_test.sh), each
// a module's output graded at an option set: every result by name, with its
// grade. Two of its cases have no counterpart, since dt runs each module
// itself: a log without the runner's closing marker, and a module count that
// disagrees with it.
func TestReadNFStest(t *testing.T) {
	entries := []known.Entry{
		{Test: "posix/read: file st_atime should be updated", Status: "fail"},
		{Test: "posix/seekdir: traceback: ValueError: NULL pointer access", Status: "fail"},
		{Test: "dio/vectored_io: traceback: Exception: Packet trace file is empty: use --trcdelay option to give tcpdump time to flush buffer to packet trace", Status: "fail", Where: map[string][]string{"mount-options": {"v4.0"}}},
		{Test: "dio/[rw]size: traceback: Exception: mount.nfs: requested NFS version or transport protocol is not supported for /tmp/dittofs-test", Status: "fail", Where: map[string][]string{"mount-options": {"v3"}}},
	}
	const emptyTrace = `    TEST: Running test 'vectored_io'
    FAIL: Traceback (most recent call last):
            File "/x/nfstest_dio", line 1238, in vectored_io
          Exception: Packet trace file is empty: use --trcdelay option to give tcpdump time to flush buffer to packet trace
1 tests (0 passed, 1 failed)
`
	const multiLine = `    TEST: Running test 'rsize'
    FAIL: Traceback (most recent call last):
            File "/x/host.py", line 432, in run_cmd
              raise Exception(self.pstderr)
          Exception: Created symlink /run/systemd/system/remote-fs.target.wants/rpc-statd.service.
          mount.nfs: requested NFS version or transport protocol is not supported for /tmp/dittofs-test

    TEST: Running test 'wsize'
    FAIL: Traceback (most recent call last):
            File "/x/host.py", line 432, in run_cmd
          Exception: mount.nfs: requested NFS version or transport protocol is not supported for /tmp/dittofs-test
2 tests (0 passed, 2 failed)
`
	for _, tc := range []struct {
		name, module, set, output string
		want                      []string // each result as "grade name"
	}{
		{"known failures only", "posix", "v4.1", `*** Verify POSIX API read() on NFSv3
    TEST: Running test 'read'
    PASS: read - read should succeed
    FAIL: read - file st_atime should be updated
    TEST: Running test 'seekdir'
    FAIL: Traceback (most recent call last):
            File "/x/nfstest_posix", line 1715, in _tell_seek_dir_test
          ValueError: NULL pointer access
3 tests (1 passed, 2 failed)
`, []string{" posix/read: read should succeed", "known posix/read: file st_atime should be updated", "known posix/seekdir: traceback: ValueError: NULL pointer access"}},
		// The tally moves between runs and "|" would end a table's cell, so
		// neither reaches the name.
		{"name normalisation", "posix", "v4.1", `    TEST: Running test 'open'
    FAIL: open - opening existent file should return an error when O_EXCL|O_CREAT is used (256 passed, 256 failed)
1 tests (0 passed, 1 failed)
`, []string{"new posix/open: opening existent file should return an error when O_EXCL+O_CREAT is used"}},
		{"new failure beside a known one", "posix", "v4.1", `    TEST: Running test 'read'
    FAIL: read - file st_atime should be updated
    FAIL: read - read should return the data written
2 tests (0 passed, 2 failed)
`, []string{"known posix/read: file st_atime should be updated", "new posix/read: read should return the data written"}},
		{"module without a tally", "lock", "v4.1", `    TEST: Running test 'btest01'
    PASS: Locking byte range should be granted
`, []string{" lock/btest01: Locking byte range should be granted", "incomplete lock: did not finish"}},
		{"module that printed only usage", "posix", "v4.1", "Usage: nfstest_posix --server <server> [options]\n", []string{"incomplete posix: did not finish"}},
		// A line the reader missed could be a failure: never green, even
		// when every failure it read is known.
		{"tally disagrees with the lines read", "posix", "v4.1", `    TEST: Running test 'read'
    PASS: read - read should succeed
    FAIL: read - file st_atime should be updated
3 tests (1 passed, 2 failed)
`, []string{" posix/read: read should succeed", "known posix/read: file st_atime should be updated", "incomplete posix: tally says 1 passed, 2 failed; parsed 1, 1"}},
		{"an entry where it applies", "dio", "v4.0", emptyTrace, []string{"known dio/vectored_io: traceback: Exception: Packet trace file is empty: use --trcdelay option to give tcpdump time to flush buffer to packet trace"}},
		{"and only there", "dio", "v4.1", emptyTrace, []string{"new dio/vectored_io: traceback: Exception: Packet trace file is empty: use --trcdelay option to give tcpdump time to flush buffer to packet trace"}},
		// A tool printing before the error, as systemd starting rpc-statd
		// on a fresh host, doesn't change the name: the type comes from the
		// message's first line, the text from its last.
		{"a multi-line exception", "dio", "v3", multiLine, []string{
			"known dio/rsize: traceback: Exception: mount.nfs: requested NFS version or transport protocol is not supported for /tmp/dittofs-test",
			"known dio/wsize: traceback: Exception: mount.nfs: requested NFS version or transport protocol is not supported for /tmp/dittofs-test"}},
		{"a different exception in a known subtest", "posix", "v4.1", `    TEST: Running test 'seekdir'
    FAIL: Traceback (most recent call last):
            File "/x/nfstest_posix", line 1715, in _tell_seek_dir_test
          OSError: [Errno 5] Input/output error
1 tests (0 passed, 1 failed)
`, []string{"new posix/seekdir: traceback: OSError: [Errno N] Input/output error"}},
		{"addresses and numbers", "dio", "v4.1", `    TEST: Running test 'basic'
    FAIL: Traceback (most recent call last):
            File "/x/host.py", line 432, in run_cmd
          Exception: mount.nfs: Connection refused for 172.17.0.2:/export on /tmp/dittofs-test_01
1 tests (0 passed, 1 failed)
`, []string{"new dio/basic: traceback: Exception: mount.nfs: Connection refused for <ip>:/export on /tmp/dittofs-test_N"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tests, err := readNFStest(report.Ran{Test: tc.module, Output: []byte(tc.output)})
			if err != nil {
				t.Fatal(err)
			}
			known.Grade(tests, true, entries, map[string]string{"mount-options": tc.set})
			var got []string
			for _, r := range tests {
				got = append(got, fmt.Sprintf("%s %s", r.Grade, r.Name))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}
