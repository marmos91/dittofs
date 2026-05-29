package snapshot

import (
	"bytes"
	"encoding/json"
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
