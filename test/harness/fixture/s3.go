package fixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// bucketName is the bucket an S3 profile's server keeps its blocks in.
//
// decision: every fixture uses this one bucket, each under a key prefix of
// its own, so fixtures side by side or one after another never see each
// other's blocks. The service keeps the bucket once the first fixture creates
// it, and a later fixture finds it there. Nothing under a fixture's prefix is
// deleted when it stops, which holds while the S3 service lives no longer
// than a run; delete the prefix on Stop once a fixture uses one that does.
const bucketName = "dittofs-dt"

// bucket is where an S3 profile's server keeps its blocks: bucketName at the
// S3 service's endpoint, under a prefix of the fixture's own.
type bucket struct {
	endpoint string
	prefix   string // ends in /
}

// newBucket creates the bucket at the endpoint of the S3 service the profile
// needs, which services, the variables of the cell's services, give. It then
// checks that the bucket takes a write and gives the same bytes back, so a
// missing or unreachable bucket fails the setup rather than leaving the server
// on its local tier alone.
//
// decision: the requests are unsigned and the server gets fixed credentials,
// which the S3 service dt services starts accepts. A profile on a real S3
// service needs signed requests and the credentials its own need gives.
func newBucket(ctx context.Context, services []string) (*bucket, error) {
	var endpoint string
	for _, kv := range services {
		if v, ok := strings.CutPrefix(kv, "LOCALSTACK_ENDPOINT="); ok {
			endpoint = strings.TrimSuffix(v, "/")
		}
	}
	if endpoint == "" {
		return nil, errors.New("an S3 profile needs service:localstack, and the cell has no LOCALSTACK_ENDPOINT")
	}
	b := &bucket{endpoint: endpoint, prefix: "dt-" + secret()[:16] + "/"}
	where := fmt.Sprintf("the bucket %s at %s", bucketName, endpoint)
	// A bucket that exists already answers 200 or 409, by the service.
	status, _, err := b.do(ctx, http.MethodPut, "", nil)
	if err != nil || (status != http.StatusOK && status != http.StatusConflict) {
		return nil, fmt.Errorf("creating %s: %s", where, answer(status, err))
	}
	probe := []byte(secret())
	key := b.prefix + "dt-probe"
	if status, _, err = b.do(ctx, http.MethodPut, key, probe); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("%s takes no write: PUT %s %s", where, key, answer(status, err))
	}
	status, got, err := b.do(ctx, http.MethodGet, key, nil)
	if err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("%s doesn't give back a write: GET %s %s", where, key, answer(status, err))
	}
	if !bytes.Equal(got, probe) {
		return nil, fmt.Errorf("%s doesn't give back a write: GET %s gave other bytes than the PUT", where, key)
	}
	return b, nil
}

// do sends one request for key in the bucket, or for the bucket itself when
// key is "", and returns the answer's status and body.
func (b *bucket) do(ctx context.Context, method, key string, body []byte) (int, []byte, error) {
	url := b.endpoint + "/" + bucketName
	if key != "" {
		url += "/" + key
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// answer says how a request went: its error, or the status it was answered.
func answer(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("answered %d", status)
}

// listURL is {bucket}: the request that lists the keys under the fixture's
// prefix, an S3 ListObjectsV2, with which a suite sees what the server put in
// the bucket.
func (b *bucket) listURL() string {
	return b.endpoint + "/" + bucketName + "?list-type=2&prefix=" + b.prefix
}

// storeConfig is the server's S3 block store configuration for the bucket.
func (b *bucket) storeConfig() map[string]any {
	return map[string]any{
		"bucket":            bucketName,
		"prefix":            b.prefix,
		"endpoint":          b.endpoint,
		"region":            "us-east-1",
		"force_path_style":  true,
		"access_key_id":     "test",
		"secret_access_key": "test",
		// The service is on a loopback or private address, which the
		// server refuses for an S3 endpoint unless this allows it.
		"allow_private_endpoint": true,
	}
}
