package snapshot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

func resetCreateFlags() {
	createName = ""
	createNoVerify = false
	createRetry = ""
	createNoWait = false
}

// TestCreate_NoWaitJSONHasIDField asserts the --no-wait JSON output carries
// an `id` field (consistent with the blocking path) so `jq '.id'` works.
func TestCreate_NoWaitJSONHasIDField(t *testing.T) {
	resetCreateFlags()
	createNoWait = true
	cmdutil.Flags.Output = "json"
	t.Cleanup(func() { cmdutil.Flags.Output = "" })

	fc := &fakeClient{
		createResp: &apiclient.CreateSnapshotResponse{SnapshotID: "snap-xyz", Share: "/archive"},
	}
	withFakeClient(t, fc)

	prev := osStdout()
	r, w := setStdout()
	defer restoreStdout(prev)

	if err := runCreate(createCmd, []string{"/archive"}); err != nil {
		t.Fatalf("runCreate: %v", err)
	}
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var obj map[string]any
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, buf.String())
	}
	if obj["id"] != "snap-xyz" {
		t.Fatalf("json id = %v, want snap-xyz (full body: %s)", obj["id"], buf.String())
	}
	if obj["state"] != "creating" {
		t.Fatalf("json state = %v, want creating", obj["state"])
	}
}

// TestCreate_FailedStateExitsNonZeroAcrossFormats asserts that a blocking
// create whose snapshot reaches the `failed` terminal state returns a
// non-nil error in EVERY output format — json/yaml/table. This is the
// exit-0-on-failure class fix: previously the json/yaml branches returned
// nil unconditionally, so `snapshot create -o json` reported a failed
// backup as success and CI could not detect it. The body is still emitted.
func TestCreate_FailedStateExitsNonZeroAcrossFormats(t *testing.T) {
	for _, format := range []string{"json", "yaml", "table"} {
		t.Run(format, func(t *testing.T) {
			resetCreateFlags()
			cmdutil.Flags.Output = format
			t.Cleanup(func() { cmdutil.Flags.Output = "" })

			fc := &fakeClient{
				createResp: &apiclient.CreateSnapshotResponse{SnapshotID: "snap-fail", Share: "/archive"},
				snapshots: map[string]*apiclient.Snapshot{
					"snap-fail": {ID: "snap-fail", Share: "/archive", State: "creating"},
				},
				waitFinalState: "failed",
			}
			withFakeClient(t, fc)

			prev := osStdout()
			r, w := setStdout()
			defer restoreStdout(prev)

			err := runCreate(createCmd, []string{"/archive"})

			_ = w.Close()
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r)

			if err == nil {
				t.Fatalf("%s: failed snapshot must return a non-nil error (non-zero exit)", format)
			}
			if !strings.Contains(err.Error(), "snap-fail") {
				t.Errorf("%s: error should reference the snapshot id; got %v", format, err)
			}
		})
	}
}
