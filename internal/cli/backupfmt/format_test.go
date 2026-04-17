package backupfmt

import (
	"strings"
	"testing"
	"time"
)

func TestShortULID_TruncatesTo8(t *testing.T) {
	id := "01HABCDEFGHJKMNPQRST" // 20-char ULID sample
	got := ShortULID(id)
	want := "01HABCDE\u2026"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if got := ShortULID("ABC"); got != "ABC" {
		t.Errorf("short input mangled: got %q", got)
	}
}

func TestTimeAgo_RelativeFormats(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{30 * time.Second, "30s ago"},
		{3 * time.Minute, "3m ago"},
		{3 * time.Hour, "3h ago"},
		{2 * 24 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		got := TimeAgoSince(now.Add(-tc.offset), now)
		if got != tc.want {
			t.Errorf("offset=%s: want %q, got %q", tc.offset, tc.want, got)
		}
	}
}

func TestRenderProgressBar_Mid(t *testing.T) {
	got := RenderProgressBar(50)
	if !strings.HasPrefix(got, "50%") {
		t.Errorf("expected 50%% prefix, got %q", got)
	}
	if !strings.Contains(got, "\u2593") || !strings.Contains(got, "\u2591") {
		t.Errorf("expected filled + empty cells, got %q", got)
	}
	if p := RenderProgressBar(-10); !strings.HasPrefix(p, "0%") {
		t.Errorf("negative pct should clamp to 0, got %q", p)
	}
	if p := RenderProgressBar(250); !strings.HasPrefix(p, "100%") {
		t.Errorf("huge pct should clamp to 100, got %q", p)
	}
}
