package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedUpdateCache(t *testing.T, home, latest string, checked time.Time) {
	t.Helper()
	data, _ := json.Marshal(updateCache{CheckedAt: checked, Latest: latest})
	if err := os.WriteFile(filepath.Join(home, "update-check.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	orig, origFetch := version, fetchLatestTag
	t.Cleanup(func() { version, fetchLatestTag = orig, origFetch; serverTag.Store("") })
	version = v
	fetchLatestTag = func(string) (string, error) { t.Fatal("unexpected network fetch"); return "", nil }
	serverTag.Store("")
}

func TestEmitJSONAddsUpdateAvailableWhenNewerKnown(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.0.0")
	seedUpdateCache(t, home, "v1.2.0", time.Now())

	type doc struct {
		Zeta  string `json:"zeta"`
		Alpha int    `json:"alpha"`
	}
	out := captureStdout(t, func() { emitJSON(doc{Zeta: "z", Alpha: 1}) })
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	ua, ok := got["update_available"].(map[string]any)
	if !ok || ua["current"] != "v1.0.0" || ua["latest"] != "v1.2.0" {
		t.Fatalf("update_available = %#v", got["update_available"])
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{\n  \"zeta\": \"z\",\n  \"alpha\": 1,\n  \"update_available\"") {
		t.Fatalf("existing keys/order changed:\n%s", out)
	}
}

func TestEmitJSONLeavesDocumentAloneWithoutUpdate(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.2.0")
	seedUpdateCache(t, home, "v1.2.0", time.Now())
	want := "{\n  \"a\": \"<b>\"\n}\n"
	if out := captureStdout(t, func() { emitJSON(map[string]string{"a": "<b>"}) }); out != "{\n  \"a\": \"\\u003cb\\u003e\"\n}\n" {
		t.Fatalf("output changed: %q (plain encoder form %q)", out, want)
	}
}

func TestWithUpdateAvailableCases(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.0.0")
	seedUpdateCache(t, home, "v1.1.0", time.Now())
	if got := string(withUpdateAvailable([]byte(`[1,2]`))); got != `[1,2]` {
		t.Fatalf("array changed: %s", got)
	}
	if got := string(withUpdateAvailable([]byte(`{"update_available":false}`))); got != `{"update_available":false}` {
		t.Fatalf("existing key overwritten: %s", got)
	}
	if got := string(withUpdateAvailable([]byte(`{}`))); got != `{"update_available":{"current":"v1.0.0","latest":"v1.1.0"}}` {
		t.Fatalf("empty object: %s", got)
	}
	// Opt-out and dev builds never add it.
	if err := saveConfig(&Config{UpdateCheckDisabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := string(withUpdateAvailable([]byte(`{"a":1}`))); got != `{"a":1}` {
		t.Fatalf("disabled check still added the field: %s", got)
	}
}

func TestTransportSetsUserAgentAndLearnsLatest(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.0.0")
	var mu sync.Mutex
	var uas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uas = append(uas, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set(latestCLIHeader, "v1.3.0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL+"/api/v1")
	apiHostOnce = sync.Once{}
	t.Cleanup(func() { apiHostOnce = sync.Once{} })

	resp, err := http.Get(srv.URL + "/api/v1/account")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("User-Agent", "custom/1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	want := "tessera-cli/v1.0.0 (" + runtimeGOOS() + "/" + runtimeGOARCH() + ")"
	if uas[0] != want || uas[1] != "custom/1" {
		t.Fatalf("user agents = %q, want [%q custom/1]", uas, want)
	}
	if got := serverLatestTag(); got != "v1.3.0" {
		t.Fatalf("server latest = %q", got)
	}
	if got := rememberServerLatest(); got != "v1.3.0" || loadUpdateCache().Latest != "v1.3.0" {
		t.Fatalf("header value not cached: %q / %q", got, loadUpdateCache().Latest)
	}
	_ = home
}

func TestLatestHeaderIgnoredFromOtherHosts(t *testing.T) {
	testConfigHome(t)
	withVersion(t, "v1.0.0")
	t.Setenv("TESSERA_BILLING_URL", "https://tessera.example/api/v1")
	apiHostOnce = sync.Once{}
	t.Cleanup(func() { apiHostOnce = sync.Once{} })
	req, _ := http.NewRequest(http.MethodGet, "https://elsewhere.example/a", nil)
	resp := &http.Response{Header: http.Header{latestCLIHeader: {"v9.0.0"}}}
	noteLatestHeader(req, resp)
	if serverLatestTag() != "" {
		t.Fatal("header from an untrusted host was used")
	}
	req, _ = http.NewRequest(http.MethodGet, "https://tessera.example/api/v1/account", nil)
	resp.Header.Set(latestCLIHeader, "not-a-version")
	noteLatestHeader(req, resp)
	if serverLatestTag() != "" {
		t.Fatal("malformed header was used")
	}
}

func TestDefaultFetchPrefersTesseraAPI(t *testing.T) {
	testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cli/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1.4.0","url":"https://example/r","update_command":"tessera update"}`))
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL+"/api/v1")
	tag, err := defaultFetchLatestTag(updateRepo)
	if err != nil || tag != "v1.4.0" {
		t.Fatalf("defaultFetchLatestTag = %q, %v", tag, err)
	}
}

func TestNewestTag(t *testing.T) {
	if got := newestTag("", "v1.2.0", "v1.10.0", "v1.9.9"); got != "v1.10.0" {
		t.Fatalf("newestTag = %q", got)
	}
	if got := newestTag("", ""); got != "" {
		t.Fatalf("newestTag of nothing = %q", got)
	}
}

// TestNoticeChild is the child process for TestUpdateNoticeOnEveryCommand.
func TestNoticeChild(t *testing.T) {
	args := os.Getenv("TESSERA_NOTICE_CHILD_ARGS")
	if args == "" {
		t.Skip("child process only")
	}
	version = "v1.0.0"
	os.Args = append([]string{"tessera"}, strings.Split(args, "\x1f")...)
	main()
	os.Exit(0)
}

func TestUpdateNoticeOnEveryCommand(t *testing.T) {
	const want = "New version v9.9.9 available (you have v1.0.0). Run 'tessera update' to install it."
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
	}{
		{"usage error", []string{"delete"}, false},
		{"unknown command", []string{"no-such-command"}, false},
		{"command help", []string{"upload", "--help"}, false},
		{"top-level help", []string{"--help"}, false},
		{"failing command", []string{"list"}, false},
		{"json command", []string{"config", "--json"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			seedUpdateCache(t, home, "v9.9.9", time.Now())
			if c.wantJSON {
				// A logged-out machine has no config; this command needs one.
				if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestNoticeChild$")
			cmd.Env = append(os.Environ(),
				"TESSERA_NOTICE_CHILD_ARGS="+strings.Join(c.args, "\x1f"),
				"TESSERA_HOME="+home,
				"TESSERA_BILLING_URL=http://127.0.0.1:1/api/v1",
				"TESSERA_NO_READINESS_NOTICE=1",
			)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			_ = cmd.Run()
			if strings.Count(stderr.String(), want) != 1 {
				t.Fatalf("stderr does not carry the notice exactly once:\n%s", stderr.String())
			}
			if strings.Contains(stdout.String(), "New version") {
				t.Fatalf("notice leaked to stdout:\n%s", stdout.String())
			}
			if c.wantJSON {
				var doc map[string]any
				if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
					t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
				}
				if doc["update_available"] == nil {
					t.Fatalf("update_available missing:\n%s", stdout.String())
				}
			}
		})
	}
}

func TestUpdateCommandsStayQuiet(t *testing.T) {
	for _, cmd := range []string{"update", "upgrade", "version", "uninstall"} {
		if !updateNoticeSkip[cmd] {
			t.Fatalf("%s should not print the background notice", cmd)
		}
	}
}

func captureStderrNotice(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	data, _ := io.ReadAll(r)
	return string(data)
}

func TestStaleCacheRefreshesInBackground(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.0.0")
	seedUpdateCache(t, home, "v1.1.0", time.Now().Add(-updateCheckTTL-time.Minute))
	calls := 0
	fetchLatestTag = func(string) (string, error) { calls++; return "v1.5.0", nil }
	out := captureStderrNotice(t, func() { startBackgroundNotices("list", nil).print() })
	if !strings.Contains(out, "New version v1.5.0 available") || calls != 1 {
		t.Fatalf("stderr = %q, calls = %d", out, calls)
	}
	if c := loadUpdateCache(); c.Latest != "v1.5.0" || time.Since(c.CheckedAt) > time.Minute {
		t.Fatalf("cache not refreshed: %+v", c)
	}
}

func TestSlowRefreshFallsBackToCachedVersion(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.0.0")
	seedUpdateCache(t, home, "v1.1.0", time.Now().Add(-updateCheckTTL-time.Minute))
	release := make(chan struct{})
	defer close(release)
	fetchLatestTag = func(string) (string, error) { <-release; return "v1.5.0", nil }
	start := time.Now()
	out := captureStderrNotice(t, func() { startBackgroundNotices("list", nil).print() })
	if !strings.Contains(out, "New version v1.1.0 available") {
		t.Fatalf("cached version not used when the refresh is slow: %q", out)
	}
	if waited := time.Since(start); waited > updateNoticeWait+time.Second {
		t.Fatalf("waited %s for the refresh", waited)
	}
}

func TestFreshCacheMakesNoNetworkCall(t *testing.T) {
	home := testConfigHome(t)
	withVersion(t, "v1.0.0") // fetchLatestTag fails the test if called
	seedUpdateCache(t, home, "v1.0.0", time.Now())
	if out := captureStderrNotice(t, func() { startBackgroundNotices("list", nil).print() }); out != "" {
		t.Fatalf("unexpected notice: %q", out)
	}
}
