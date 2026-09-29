//go:build e2e

package live

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

// canaryResult is the JSON summary TestLiveCanary_SMB writes to
// DITTOFS_E2E_LIVE_RESULT.
type canaryResult struct {
	Time            string                  `json:"time"`
	Run             string                  `json:"run"`
	Status          string                  `json:"status"`
	Step            string                  `json:"step"`
	Share           string                  `json:"share"`
	Files           int                     `json:"files"`
	Bytes           int64                   `json:"bytes"`
	UploadedObjects int                     `json:"uploaded_objects"`
	RcloneSize      map[string]objectsCount `json:"rclone_size"`
	Seconds         map[string]float64      `json:"seconds"`
}

type objectsCount struct {
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// TestLiveCanary_SMB checks the data path of a live share end to end, through the
// Linux kernel SMB client:
//
//	write -> read back -> list -> upload (new objects under the share's prefix) ->
//	evict the server's caches and read again from the remote -> delete ->
//	GC with grace 0 -> the objects are gone
//
// It counts the share's prefix (objects and bytes, what `rclone size` reports) at
// four checkpoints. Before writing it must be 0; a failed earlier run's leftovers
// are removed and GC'd first. After the upload it holds the new objects. After the
// delete it is unchanged, because a delete alone does not remove remote objects.
// After GC it must be 0 again. The files are random: repeated content stays up to
// an hour after a delete (the dedup adoption guard, which grace 0 does not override).
func TestLiveCanary_SMB(t *testing.T) {
	tg := FromEnv(t)
	admin := tg.Admin(t)
	bucket := tg.Bucket(t)
	mnt := tg.MountSMB(t)

	started := time.Now()
	run := "run-" + started.UTC().Format("20060102T150405Z")
	res := &canaryResult{Run: run, Share: tg.Share, RcloneSize: map[string]objectsCount{}, Seconds: map[string]float64{}}
	if tg.ResultFile != "" {
		t.Cleanup(func() {
			res.Time = time.Now().UTC().Format(time.RFC3339)
			res.Status = "PASS"
			if t.Failed() {
				res.Status = "FAIL"
			}
			res.Seconds["total"] = time.Since(started).Seconds()
			if b, err := json.Marshal(res); err == nil {
				_ = os.WriteFile(tg.ResultFile, append(b, '\n'), 0o644)
			}
		})
	}

	// Each step is a subtest. The first failure stops the run, so the report names
	// the step that broke.
	step := func(name string, fn func(t *testing.T)) {
		res.Step = name
		start := time.Now()
		ok := t.Run(name, fn)
		res.Seconds[name] = time.Since(start).Seconds()
		if !ok {
			t.FailNow()
		}
	}
	checkpoint := func(t *testing.T, name string) Objects {
		t.Helper()
		o := tg.Objects(t, bucket)
		res.RcloneSize[name] = objectsCount{Objects: o.Count, Bytes: o.Bytes}
		t.Logf("%s/%s [%s]: %d object(s), %d bytes", tg.S3Bucket, tg.S3Prefix, name, o.Count, o.Bytes)
		return o
	}
	gcGrace0 := func(t *testing.T) {
		t.Helper()
		require.NoError(t, helpers.TriggerBlockGC(t, admin, tg.Share, "--grace-period", "0"))
	}

	sizesMiB := []int{1, 4, 16, 33}
	runDir := filepath.Join(mnt, run)
	payloads := map[string][]byte{}
	var written Objects

	step("preflight", func(t *testing.T) {
		_, err := admin.Run("share", "show", tg.Share)
		require.NoError(t, err, "share %s", tg.Share)
	})

	step("leftovers", func(t *testing.T) {
		entries, err := os.ReadDir(mnt)
		require.NoError(t, err)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "run-") {
				require.NoError(t, os.RemoveAll(filepath.Join(mnt, e.Name())))
				t.Logf("removed %s, left by an earlier run", e.Name())
			}
		}
	})

	step("before", func(t *testing.T) {
		if checkpoint(t, "before").Count == 0 {
			return
		}
		t.Logf("prefix not empty before writing: GC (grace 0) to clear an earlier run's objects")
		gcGrace0(t)
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if checkpoint(t, "before").Count == 0 {
				return
			}
		}
		t.Fatalf("the share's prefix still holds %d object(s) before writing (should be 0)", res.RcloneSize["before"].Objects)
	})

	step("write", func(t *testing.T) {
		require.NoError(t, os.Mkdir(runDir, 0o755))
		for _, m := range sizesMiB {
			name := fmt.Sprintf("f%dMiB.bin", m)
			data := make([]byte, m<<20)
			_, err := rand.Read(data)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(runDir, name), data, 0o644), "write %s", name)
			payloads[name] = data
			res.Files++
			res.Bytes += int64(len(data))
		}
		t.Logf("wrote %d files, %d bytes", res.Files, res.Bytes)
	})

	readAll := func(t *testing.T, what string) {
		t.Helper()
		for name, want := range payloads {
			got, err := os.ReadFile(filepath.Join(runDir, name))
			require.NoError(t, err, "%s %s", what, name)
			require.True(t, bytes.Equal(got, want), "%s %s: content differs", what, name)
		}
	}

	step("read", func(t *testing.T) { readAll(t, "read") })

	step("list", func(t *testing.T) {
		entries, err := os.ReadDir(runDir)
		require.NoError(t, err)
		require.Len(t, entries, len(payloads))
		for name, data := range payloads {
			fi, err := os.Stat(filepath.Join(runDir, name))
			require.NoError(t, err, "stat %s", name)
			require.Equal(t, int64(len(data)), fi.Size(), "size of %s", name)
		}
	})

	step("upload", func(t *testing.T) {
		took := WaitUploaded(t, admin, tg.Share, 5*time.Minute)
		written = checkpoint(t, "written")
		require.Positive(t, written.Count, "no object appeared under %s/%s", tg.S3Bucket, tg.S3Prefix)
		require.GreaterOrEqual(t, written.Bytes, res.Bytes, "the new objects hold fewer bytes than were written")
		res.UploadedObjects = written.Count
		t.Logf("uploaded in %s", took.Round(time.Second))
	})

	step("cold-read", func(t *testing.T) {
		require.NoError(t, helpers.EvictBlocks(t, admin, tg.Share))
		readAll(t, "cold read")
	})

	step("delete", func(t *testing.T) {
		require.NoError(t, os.RemoveAll(runDir))
		require.NoDirExists(t, runDir)
		if deleted := checkpoint(t, "deleted"); deleted.Count != written.Count {
			t.Logf("note: %d -> %d object(s) between the upload and the delete, before any GC", written.Count, deleted.Count)
		}
	})

	step("gc", func(t *testing.T) {
		gcGrace0(t)
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if o := checkpoint(t, "gc"); o.Count == 0 && o.Bytes == 0 {
				return
			}
		}
		o := res.RcloneSize["gc"]
		t.Fatalf("after GC the share's prefix still holds %d object(s), %d bytes (should be 0)", o.Objects, o.Bytes)
	})
}
