package block

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/pflag"
)

func TestBuildCompressionBlock(t *testing.T) {
	cases := []struct {
		algo     string
		wantAlgo string // "" → expect nil block
		wantErr  string // substring; "" → no error
	}{
		{"", "", ""},
		{"zstd", "zstd", ""},
		{"lz4", "lz4", ""},
		{"snappy", "", "invalid --compression"},
		{"none", "", "invalid --compression"},
		{"ZSTD", "", "invalid --compression"},
	}
	for _, tc := range cases {
		t.Run(tc.algo, func(t *testing.T) {
			block, err := buildCompressionBlock(tc.algo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantAlgo == "" {
				if block != nil {
					t.Fatalf("expected nil block for empty flag, got %#v", block)
				}
				return
			}
			if got, _ := block["algo"].(string); got != tc.wantAlgo {
				t.Fatalf("algo=%v, want %s", block["algo"], tc.wantAlgo)
			}
		})
	}
}

func TestBuildRemoteConfig_S3_CompressionMergesIn(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", s3Fields{bucket: "bucket", region: "us-east-1", accessKey: "AK", secretKey: "SK"}, suppliedSet(), "zstd", 0, encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", cfg)
	}
	comp, ok := m["compression"].(map[string]any)
	if !ok {
		t.Fatalf("missing compression block in config: %#v", m)
	}
	if comp["algo"] != "zstd" {
		t.Fatalf("algo=%v, want zstd", comp["algo"])
	}
}

func TestBuildRemoteConfig_S3_NoCompressionByDefault(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", s3Fields{bucket: "bucket", region: "us-east-1", accessKey: "AK", secretKey: "SK"}, suppliedSet(), "", 0, encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	if _, present := m["compression"]; present {
		t.Fatalf("compression key should be absent when flag empty: %#v", m)
	}
}

func TestBuildRemoteConfig_S3_RejectsInvalidAlgo(t *testing.T) {
	_, err := buildRemoteConfig("s3", "", s3Fields{bucket: "bucket", region: "us-east-1", accessKey: "AK", secretKey: "SK"}, suppliedSet(), "gzip", 0, encryptionFlags{})
	if err == nil || !strings.Contains(err.Error(), "invalid --compression") {
		t.Fatalf("err=%v, want invalid --compression error", err)
	}
}

func TestBuildRemoteConfig_S3_ParallelUploadsMergesIn(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", s3Fields{bucket: "bucket", region: "us-east-1", accessKey: "AK", secretKey: "SK"}, suppliedSet(), "", 8, encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", cfg)
	}
	if m["parallel_uploads"] != 8 {
		t.Fatalf("parallel_uploads=%v, want 8", m["parallel_uploads"])
	}
}

func TestBuildRemoteConfig_S3_NoParallelUploadsByDefault(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", s3Fields{bucket: "bucket", region: "us-east-1", accessKey: "AK", secretKey: "SK"}, suppliedSet(), "", 0, encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	if _, present := m["parallel_uploads"]; present {
		t.Fatalf("parallel_uploads key should be absent when flag is 0: %#v", m)
	}
}

func TestBuildEncryptionBlock_Disabled(t *testing.T) {
	block, err := buildEncryptionBlock(encryptionFlags{})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if block != nil {
		t.Fatalf("expected nil block, got %#v", block)
	}
}

func TestBuildEncryptionBlock_Local(t *testing.T) {
	block, err := buildEncryptionBlock(encryptionFlags{
		AEAD:    "aes-256-gcm",
		KeyKind: "local",
		KeyFile: "/etc/dittofs/keys/share.key",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if block["aead"] != "aes-256-gcm" {
		t.Errorf("aead=%v", block["aead"])
	}
	key, _ := block["key"].(map[string]any)
	if key["kind"] != "local" || key["file"] != "/etc/dittofs/keys/share.key" {
		t.Errorf("key block: %#v", key)
	}
}

func TestBuildEncryptionBlock_KMIP(t *testing.T) {
	block, err := buildEncryptionBlock(encryptionFlags{
		AEAD:       "chacha20-poly1305",
		KeyKind:    "kmip",
		KMIPHost:   "kms.example.com:5696",
		KMIPCA:     "/etc/dittofs/kmip/ca.pem",
		KMIPCert:   "/etc/dittofs/kmip/client.pem",
		KMIPKey:    "/etc/dittofs/kmip/client.key",
		KMIPKeyUID: "abcd-1234",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if block["aead"] != "chacha20-poly1305" {
		t.Errorf("aead=%v", block["aead"])
	}
	key, _ := block["key"].(map[string]any)
	if key["kind"] != "kmip" || key["endpoint"] != "kms.example.com:5696" || key["key_uid"] != "abcd-1234" || key["server_ca"] != "/etc/dittofs/kmip/ca.pem" {
		t.Errorf("key block: %#v", key)
	}
}

func TestBuildEncryptionBlock_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		flags   encryptionFlags
		wantSub string
	}{
		{"missing-aead-with-key", encryptionFlags{KeyKind: "local", KeyFile: "/x"}, "--encryption-aead is required"},
		{"unknown-aead", encryptionFlags{AEAD: "rc4"}, "invalid --encryption-aead"},
		{"local-missing-file", encryptionFlags{AEAD: "aes-256-gcm", KeyKind: "local"}, "--encryption-key-file is required"},
		{"kmip-missing-host", encryptionFlags{AEAD: "aes-256-gcm", KeyKind: "kmip", KMIPCert: "/c", KMIPKey: "/k", KMIPKeyUID: "u"}, "kmip-endpoint"},
		{"unknown-kind", encryptionFlags{AEAD: "aes-256-gcm", KeyKind: "vault"}, "invalid --encryption-key-kind"},
		{"missing-kind", encryptionFlags{AEAD: "aes-256-gcm"}, "--encryption-key-kind is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildEncryptionBlock(tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestBuildRemoteConfig_S3_EncryptionMergesIn(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", s3Fields{bucket: "bucket", region: "us-east-1", accessKey: "AK", secretKey: "SK"}, suppliedSet(), "", 0, encryptionFlags{
		AEAD:    "aes-256-gcm",
		KeyKind: "local",
		KeyFile: "/etc/dittofs/share.key",
	})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	enc, ok := m["encryption"].(map[string]any)
	if !ok {
		t.Fatalf("missing encryption block: %#v", m)
	}
	if enc["aead"] != "aes-256-gcm" {
		t.Errorf("aead=%v", enc["aead"])
	}
}

// blockAddServer records the store config `store block add` sends.
type blockAddServer struct {
	*httptest.Server
	config map[string]any
}

func newBlockAddServer(t *testing.T) *blockAddServer {
	t.Helper()
	s := &blockAddServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/store/block" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Config string `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.config = nil
		if req.Config != "" {
			_ = json.Unmarshal([]byte(req.Config), &s.config)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(apiclient.BlockStore{ID: "id-1", Name: req.Name, Type: req.Type})
	}))
	t.Cleanup(s.Close)
	return s
}

// resetAddFlags puts every addCmd flag back to its default and unnamed state.
func resetAddFlags() {
	addCmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
}

// runAddWith runs `store block add` with args, and no other flags, against s.
func runAddWith(t *testing.T, s *blockAddServer, args ...string) error {
	t.Helper()
	withGCTestServer(t, s.URL)
	resetAddFlags()
	t.Cleanup(resetAddFlags)
	if err := addCmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	var err error
	captureStdoutBlock(t, func() { err = runAdd(addCmd, nil) })
	return err
}

// Every flag `store block add` takes alongside --config must reach the store
// the server is asked to create. --config used to be sent as given, so a
// store asked for with --compression zstd was created uncompressed, and the
// command still succeeded.
func TestAddCmd_ConfigKeepsEveryFlag(t *testing.T) {
	cases := []struct {
		args []string
		key  string
		want any // the value under key, as the server decodes it
	}{
		{[]string{"--compression", "zstd"}, "compression", map[string]any{"algo": "zstd"}},
		{[]string{"--encryption-aead", "aes-256-gcm", "--encryption-key-kind", "local", "--encryption-key-file", "/k"},
			"encryption", map[string]any{"aead": "aes-256-gcm", "key": map[string]any{"kind": "local", "file": "/k"}}},
		{[]string{"--parallel-uploads", "8"}, "parallel_uploads", float64(8)},
		{[]string{"--bucket", "b"}, "bucket", "b"},
		{[]string{"--region", "eu-west-1"}, "region", "eu-west-1"},
		{[]string{"--endpoint", "http://127.0.0.1:9000"}, "endpoint", "http://127.0.0.1:9000"},
		{[]string{"--prefix", "p/"}, "prefix", "p/"},
		{[]string{"--access-key", "AK"}, "access_key_id", "AK"},
		{[]string{"--secret-key", "SK"}, "secret_access_key", "SK"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			s := newBlockAddServer(t)
			args := append([]string{"--name", "s", "--type", "s3", "--config", `{"allow_private_endpoint":true}`}, tc.args...)
			if err := runAddWith(t, s, args...); err != nil {
				t.Fatalf("store block add %q: %v", args, err)
			}
			if !reflect.DeepEqual(s.config[tc.key], tc.want) {
				t.Errorf("config[%q] = %#v, want %#v; config sent: %#v", tc.key, s.config[tc.key], tc.want, s.config)
			}
			if s.config["allow_private_endpoint"] != true {
				t.Errorf("the --config keys were lost: %#v", s.config)
			}
		})
	}
}

// --region has a default, so leaving it off must not override the region the
// JSON sets, nor add one the JSON leaves out.
func TestAddCmd_ConfigKeepsItsOwnRegion(t *testing.T) {
	s := newBlockAddServer(t)
	if err := runAddWith(t, s, "--name", "s", "--type", "s3", "--config", `{"bucket":"b","region":"eu-central-1"}`); err != nil {
		t.Fatalf("store block add: %v", err)
	}
	if s.config["region"] != "eu-central-1" {
		t.Errorf("region = %v, want the JSON's eu-central-1", s.config["region"])
	}
}

// A memory store applies compression, encryption and the upload cap too, so
// its flags must reach it with or without --config.
func TestAddCmd_MemoryStoreKeepsItsFlags(t *testing.T) {
	s := newBlockAddServer(t)
	if err := runAddWith(t, s, "--name", "m", "--type", "memory", "--compression", "lz4", "--parallel-uploads", "4"); err != nil {
		t.Fatalf("store block add: %v", err)
	}
	if !reflect.DeepEqual(s.config["compression"], map[string]any{"algo": "lz4"}) || s.config["parallel_uploads"] != float64(4) {
		t.Errorf("config sent: %#v, want compression lz4 and parallel_uploads 4", s.config)
	}

	s = newBlockAddServer(t)
	if err := runAddWith(t, s, "--name", "m", "--type", "memory"); err != nil {
		t.Fatalf("store block add: %v", err)
	}
	if s.config != nil {
		t.Errorf("a memory store without flags sent config %#v, want none", s.config)
	}
}

// A flag that agrees with --config is accepted; one that contradicts it is
// refused, naming the flag and the key but not the values, which may be
// credentials.
func TestBuildRemoteConfig_JSONConfigConflicts(t *testing.T) {
	supplied := suppliedSet("bucket", "secret-key")
	same := s3Fields{bucket: "b", secretKey: "SK"}
	cfg, err := buildRemoteConfig("s3", `{"bucket":"b","secret_access_key":"SK","parallel_uploads":8}`, same, supplied, "", 8, encryptionFlags{})
	if err != nil {
		t.Fatalf("flags equal to the JSON's values: %v", err)
	}
	if m, _ := cfg.(map[string]any); m["parallel_uploads"] != 8 {
		t.Errorf("parallel_uploads = %#v, want 8", m["parallel_uploads"])
	}

	cases := []struct {
		name     string
		json     string
		s3       s3Fields
		compress string
		wantSub  string
	}{
		{"bucket", `{"bucket":"other"}`, s3Fields{bucket: "b"}, "", `--bucket conflicts with "bucket"`},
		{"secret", `{"secret_access_key":"OLD-SECRET"}`, s3Fields{secretKey: "NEW-SECRET"}, "", `--secret-key conflicts with "secret_access_key"`},
		{"compression", `{"compression":{"algo":"lz4"}}`, s3Fields{}, "zstd", `--compression conflicts with "compression"`},
		{"not-an-object", `["x"]`, s3Fields{}, "zstd", "--config must be a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildRemoteConfig("s3", tc.json, tc.s3, supplied, tc.compress, 0, encryptionFlags{})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error discloses a credential: %v", err)
			}
		})
	}
}

// recordingAsker answers every prompt with a canned value and records the
// labels it was asked, so the per-field gating can be inspected without a
// terminal.
type recordingAsker struct {
	asked  []string
	answer string
}

func (r *recordingAsker) asker() *s3Asker {
	record := func(label string) (string, error) {
		r.asked = append(r.asked, label)
		return r.answer, nil
	}
	return &s3Asker{
		required:    record,
		optional:    record,
		secret:      record,
		withDefault: func(label, _ string) (string, error) { return record(label) },
	}
}

func (r *recordingAsker) wasAsked(substr string) bool {
	for _, label := range r.asked {
		if strings.Contains(label, substr) {
			return true
		}
	}
	return false
}

// suppliedSet turns a list of flag names into the predicate runAdd builds from
// cmd.Flags().Changed.
func suppliedSet(names ...string) func(string) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) bool { return set[name] }
}

// Naming the bucket on the command line must not answer any other question:
// the endpoint decides whether the store talks to a gateway or to AWS, and it
// has to be asked on its own.
func TestPromptMissingS3Fields_BucketDoesNotSuppressEndpoint(t *testing.T) {
	rec := &recordingAsker{answer: "https://s3.example.invalid"}
	fields := s3Fields{bucket: "my-bucket", region: "us-east-1"}

	if err := promptMissingS3Fields(&fields, suppliedSet("bucket"), rec.asker()); err != nil {
		t.Fatalf("promptMissingS3Fields: %v", err)
	}

	if !rec.wasAsked("endpoint") {
		t.Errorf("endpoint was never asked for; questions asked: %q", rec.asked)
	}
	if fields.endpoint != "https://s3.example.invalid" {
		t.Errorf("endpoint=%q, want the answered value", fields.endpoint)
	}
	for _, want := range []string{"region", "Key prefix", "Access key", "Secret access key"} {
		if !rec.wasAsked(want) {
			t.Errorf("%q was never asked for; questions asked: %q", want, rec.asked)
		}
	}
	if rec.wasAsked("bucket") {
		t.Errorf("bucket was supplied on the command line and must not be asked: %q", rec.asked)
	}
	if fields.bucket != "my-bucket" {
		t.Errorf("bucket=%q, want the supplied value untouched", fields.bucket)
	}
}

// A flag the operator named is never re-asked, even when its value is empty.
func TestPromptMissingS3Fields_AsksNothingWhenEveryFlagIsNamed(t *testing.T) {
	rec := &recordingAsker{answer: "unexpected"}
	fields := s3Fields{bucket: "b", region: "eu-west-1", accessKey: "AK", secretKey: "SK"}

	err := promptMissingS3Fields(&fields,
		suppliedSet("bucket", "region", "prefix", "endpoint", "access-key", "secret-key"), rec.asker())
	if err != nil {
		t.Fatalf("promptMissingS3Fields: %v", err)
	}
	if len(rec.asked) != 0 {
		t.Errorf("asked %q, want nothing", rec.asked)
	}
	if fields.endpoint != "" {
		t.Errorf("endpoint=%q, want the explicitly empty value preserved", fields.endpoint)
	}
}

// Without a terminal there is nobody to answer, so a missing required value is
// an error naming the flag rather than a prompt that blocks or reads EOF.
func TestPromptMissingS3Fields_NonInteractiveReportsMissingFlags(t *testing.T) {
	fields := s3Fields{region: "us-east-1"}
	err := promptMissingS3Fields(&fields, suppliedSet(), nil)
	if err == nil {
		t.Fatal("expected an error when nothing was supplied and stdin is not a terminal")
	}
	for _, want := range []string{"--bucket", "--access-key", "--secret-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	fields = s3Fields{bucket: "b", accessKey: "AK", secretKey: "SK", region: "us-east-1"}
	if err := promptMissingS3Fields(&fields,
		suppliedSet("bucket", "access-key", "secret-key"), nil); err != nil {
		t.Fatalf("fully-flagged non-interactive run: %v", err)
	}
}

func TestS3TargetDescription(t *testing.T) {
	if got := s3TargetDescription(map[string]any{"bucket": "b"}); !strings.Contains(got, "AWS") {
		t.Errorf("an absent endpoint must read as AWS, got %q", got)
	}
	got := s3TargetDescription(map[string]any{"endpoint": "https://s3.fr-par.scw.cloud"})
	if got != "https://s3.fr-par.scw.cloud" {
		t.Errorf("target=%q, want the configured endpoint", got)
	}
}

// The gating looks flags up by name, so a typo would compile, pass every other
// test here, and silently stop asking about a field — the exact shape of the
// bug this gating exists to prevent. Pin both branches' names against the flag
// set the command actually registers.
func TestPromptMissingS3Fields_QueriesOnlyRealFlags(t *testing.T) {
	check := func(name string) {
		t.Helper()
		if addCmd.Flags().Lookup(name) == nil {
			t.Errorf("promptMissingS3Fields asks about %q, which `store block add` does not define", name)
		}
	}

	rec := &recordingAsker{answer: "x"}
	if err := promptMissingS3Fields(&s3Fields{}, func(name string) bool {
		check(name)
		return false
	}, rec.asker()); err != nil {
		t.Fatalf("interactive branch: %v", err)
	}

	if err := promptMissingS3Fields(&s3Fields{}, func(name string) bool {
		check(name)
		return true
	}, nil); err != nil {
		t.Fatalf("non-interactive branch: %v", err)
	}
}

// The echo shares stdout with the created store, which the caller prints as a
// JSON or YAML document. Assert on the stream a machine reader actually
// consumes: prose ahead of the document makes the whole stream unparseable, so
// the parse is the assertion and TestS3TargetDescription above cannot catch it.
func TestEchoS3Target_LeavesMachineOutputParseable(t *testing.T) {
	config := map[string]any{
		"bucket":   "test-bucket",
		"region":   "us-east-1",
		"endpoint": "http://localhost:4566",
	}
	store := map[string]any{"id": "febf65ff", "name": "block_s3", "type": "s3"}

	for _, format := range []output.Format{output.FormatJSON, output.FormatYAML} {
		t.Run(string(format), func(t *testing.T) {
			var buf bytes.Buffer
			echoS3Target(&buf, format, config)
			if err := json.NewEncoder(&buf).Encode(store); err != nil {
				t.Fatalf("encode store: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not parseable under --output %s: %v\nraw: %s",
					format, err, buf.String())
			}
			if got["name"] != "block_s3" {
				t.Errorf("name=%v, want block_s3", got["name"])
			}
		})
	}

	var buf bytes.Buffer
	echoS3Target(&buf, output.FormatTable, config)
	if !strings.Contains(buf.String(), "http://localhost:4566") {
		t.Errorf("table output must still name the target, got %q", buf.String())
	}
}
