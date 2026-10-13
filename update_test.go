package main

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.2.3-rc1", "v1.2.3", 0}, // prerelease suffix ignored
		{"dev", "v1.0.0", -1},       // unparsable -> 0.0.0
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// The proof the owner asked for: when the release tag differs from the build,
// the CLI reports it (and does not report when they match).
func TestUpdateNoticeWhenVersionDiffers(t *testing.T) {
	testConfigHome(t)
	origV, origFetch := version, fetchLatestTag
	defer func() { version, fetchLatestTag = origV, origFetch }()

	version = "v1.0.0"
	calls := 0
	fetchLatestTag = func(repo string) (string, error) {
		calls++
		if repo != updateRepo {
			t.Fatalf("unexpected repo %q", repo)
		}
		return "v1.2.3", nil
	}

	msg := updateNotice(&Config{})
	if !strings.Contains(msg, "v1.2.3") || !strings.Contains(msg, "v1.0.0") {
		t.Fatalf("notice = %q, want it to mention both versions", msg)
	}
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls)
	}

	// Throttled: a second call inside the TTL must not probe again.
	_ = updateNotice(&Config{})
	if calls != 1 {
		t.Fatalf("throttle failed: fetch calls = %d, want 1", calls)
	}
}

func TestUpdateNoticeSilentOnSameVersion(t *testing.T) {
	testConfigHome(t)
	origV, origFetch := version, fetchLatestTag
	defer func() { version, fetchLatestTag = origV, origFetch }()

	version = "v1.2.3"
	fetchLatestTag = func(string) (string, error) { return "v1.2.3", nil }
	if msg := updateNotice(&Config{}); msg != "" {
		t.Fatalf("notice = %q, want empty when versions match", msg)
	}
}

func TestUpdateCheckDisabledSkipsNetwork(t *testing.T) {
	testConfigHome(t)
	origV, origFetch := version, fetchLatestTag
	defer func() { version, fetchLatestTag = origV, origFetch }()

	version = "v1.0.0"
	calls := 0
	fetchLatestTag = func(string) (string, error) { calls++; return "v9.9.9", nil }

	if msg := updateNotice(&Config{UpdateCheckDisabled: true}); msg != "" {
		t.Fatalf("notice = %q, want empty when disabled", msg)
	}
	if calls != 0 {
		t.Fatalf("disabled check still fetched: calls = %d", calls)
	}
}

func TestUpdateNoticeSilentForDevBuild(t *testing.T) {
	testConfigHome(t)
	origV, origFetch := version, fetchLatestTag
	defer func() { version, fetchLatestTag = origV, origFetch }()

	version = "dev"
	calls := 0
	fetchLatestTag = func(string) (string, error) { calls++; return "v9.9.9", nil }
	if msg := updateNotice(&Config{}); msg != "" {
		t.Fatalf("notice = %q, want empty for a dev build", msg)
	}
	if calls != 0 {
		t.Fatalf("dev build fetched: calls = %d", calls)
	}
}
