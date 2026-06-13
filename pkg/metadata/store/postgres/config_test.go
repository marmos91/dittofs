package postgres

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConnectionString_URLFormat(t *testing.T) {
	cfg := &PostgresMetadataStoreConfig{
		Host:           "db.example.com",
		Port:           5432,
		Database:       "mydb",
		User:           "alice",
		Password:       "s3cr3t",
		SSLMode:        "verify-full",
		ConnectTimeout: 5 * time.Second,
	}
	dsn := cfg.ConnectionString()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("ConnectionString() produced unparseable URL: %v", err)
	}
	if u.Scheme != "postgres" {
		t.Errorf("scheme = %q, want postgres", u.Scheme)
	}
	if got := u.Hostname(); got != "db.example.com" {
		t.Errorf("host = %q, want db.example.com", got)
	}
	if got := u.Port(); got != "5432" {
		t.Errorf("port = %q, want 5432", got)
	}
	if got := strings.TrimPrefix(u.Path, "/"); got != "mydb" {
		t.Errorf("database = %q, want mydb", got)
	}
	if got := u.User.Username(); got != "alice" {
		t.Errorf("user = %q, want alice", got)
	}
	pass, _ := u.User.Password()
	if pass != "s3cr3t" {
		t.Errorf("password round-tripped = %q, want s3cr3t", pass)
	}
	if got := u.Query().Get("sslmode"); got != "verify-full" {
		t.Errorf("sslmode = %q, want verify-full", got)
	}
	if got := u.Query().Get("connect_timeout"); got != "5" {
		t.Errorf("connect_timeout = %q, want 5", got)
	}
}

// TestConnectionString_InjectionPrevented is the fails-before/passes-after test.
//
// Before the fix, ConnectionString() interpolated raw fields into a libpq
// key=value string. A password of "s3cr3t sslmode=disable" produced
// "...password=s3cr3t sslmode=disable sslmode=verify-full ..." — libpq/pgx
// splits on whitespace and the FIRST sslmode wins, so the attacker-controlled
// "disable" silently overrode the operator-configured "verify-full",
// downgrading TLS.
//
// After the fix the DSN is a URL: the password is part of the userinfo
// component (delimited by "@"), so pgx parses it as a single opaque value and
// the authoritative sslmode query parameter is unaffected. We assert this by
// parsing the produced DSN through pgx itself: the password must round-trip
// verbatim AND TLS must remain enabled (verify-full => non-nil TLSConfig;
// an injected sslmode=disable would yield a nil TLSConfig).
func TestConnectionString_InjectionPrevented(t *testing.T) {
	cfg := &PostgresMetadataStoreConfig{
		Host:           "localhost",
		Port:           5432,
		Database:       "mydb",
		User:           "alice",
		Password:       "s3cr3t sslmode=disable",
		SSLMode:        "verify-full",
		ConnectTimeout: 5 * time.Second,
	}
	dsn := cfg.ConnectionString()

	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig(%q): %v", dsn, err)
	}
	if got := pc.ConnConfig.Password; got != "s3cr3t sslmode=disable" {
		t.Errorf("password parsed by pgx = %q, want %q", got, "s3cr3t sslmode=disable")
	}
	// verify-full requires TLS, so pgx populates TLSConfig. If the injected
	// "sslmode=disable" had taken effect, TLSConfig would be nil.
	if pc.ConnConfig.TLSConfig == nil {
		t.Errorf("sslmode injection succeeded: TLS is disabled despite configured verify-full")
	}
}

// TestConnectionString_SpecialCharsInUser tests that @ and / in the username and
// password are also safe (other common injection / parse-break vectors).
func TestConnectionString_SpecialCharsInUser(t *testing.T) {
	cfg := &PostgresMetadataStoreConfig{
		Host:           "localhost",
		Port:           5432,
		Database:       "mydb",
		User:           "alice@domain.com",
		Password:       "p@ss/word",
		SSLMode:        "disable",
		ConnectTimeout: 5 * time.Second,
	}
	dsn := cfg.ConnectionString()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if got := u.User.Username(); got != "alice@domain.com" {
		t.Errorf("username round-tripped = %q", got)
	}
	pass, ok := u.User.Password()
	if !ok || pass != "p@ss/word" {
		t.Errorf("password round-tripped = %q, ok=%v", pass, ok)
	}
}
