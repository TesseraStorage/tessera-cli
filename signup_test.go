package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSignupAPI implements billingAPI with only CreateAgent wired. The
// embedded nil interface satisfies the other methods and panics if one is
// called, which signup must never do (no credentials, no storage endpoints).
type fakeSignupAPI struct {
	billingAPI
	accountID    string
	apiKey       string
	err          error
	name         string
	contactEmail string
	calls        int
}

// Account is read after the automatic login for the readiness line.
func (f *fakeSignupAPI) Account() (map[string]any, error) {
	return nil, errors.New("not available in fake")
}

func (f *fakeSignupAPI) CreateAgent(name, contactEmail string) (string, string, error) {
	f.name, f.contactEmail = name, contactEmail
	f.calls++
	if f.err != nil {
		return "", "", f.err
	}
	return f.accountID, f.apiKey, nil
}

// stubBillingClient swaps the client-construction seam for fake.
func stubBillingClient(t *testing.T, fake billingAPI) {
	t.Helper()
	orig := newBillingClient
	newBillingClient = func(*Config) billingAPI { return fake }
	origReg := signupRegister
	signupRegister = func(cfg *Config, phrase, accountID string) error {
		cfg.AppID = "app-" + accountID
		cfg.AppKey = strings.Repeat("ab", 32)
		return saveConfig(cfg)
	}
	t.Cleanup(func() { newBillingClient = orig; signupRegister = origReg })
}

// TestSignupValueFlagsRegistered guards the positional() trap the plan calls
// out: both signup flags take separate values, so takesValue must know them.
func TestSignupValueFlagsRegistered(t *testing.T) {
	for _, flag := range []string{"--name", "--contact-email"} {
		if !takesValue(flag) {
			t.Errorf("takesValue(%q) = false; positional() would swallow its value", flag)
		}
	}
	if takesValue("--force") {
		t.Error("takesValue(--force) = true; --force is a boolean and must not consume a value")
	}
}

// TestCmdSignupColdStart is the core promise: with no config file at all,
// signup creates the agent, saves a 0600 config with the default URLs and the
// returned key, and prints the key exactly once with the store-it warning.
func TestCmdSignupColdStart(t *testing.T) {
	pinStdoutTerminal(t, true)
	home := testConfigHome(t)
	cp := filepath.Join(home, configFileName)
	if _, err := os.Stat(cp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: %s should not exist (err=%v)", cp, err)
	}
	fake := &fakeSignupAPI{accountID: "AGNT-1", apiKey: "tsk_alpha_secret"}
	stubBillingClient(t, fake)

	out := captureStdout(t, func() { cmdSignup(nil) })
	if fake.calls != 1 {
		t.Fatalf("CreateAgent calls = %d, want 1", fake.calls)
	}

	fi, err := os.Stat(cp)
	if err != nil {
		t.Fatalf("config file was not saved: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %o, want 0600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if raw["indexer_url"] != defaultIndexer {
		t.Errorf("indexer_url = %v, want %q", raw["indexer_url"], defaultIndexer)
	}
	if raw["billing_api_url"] != defaultBillingURL {
		t.Errorf("billing_api_url = %v, want %q", raw["billing_api_url"], defaultBillingURL)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.BillingAgentKey != "tsk_alpha_secret" {
		t.Errorf("BillingAgentKey = %q, want the returned key", cfg.BillingAgentKey)
	}

	if got := strings.Count(out, "tsk_alpha_secret"); got != 1 {
		t.Errorf("key printed %d times, want exactly once:\n%s", got, out)
	}
	if !strings.Contains(out, "AGNT-1") {
		t.Errorf("account id missing from output:\n%s", out)
	}
	if !strings.Contains(out, "will not be shown again") {
		t.Errorf("store-it warning missing from output:\n%s", out)
	}
}

// TestCmdSignupSendsNameAndContactEmail covers both flag forms and the
// omitted case, which must send empty strings rather than inventing values.
func TestCmdSignupSendsNameAndContactEmail(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantName string
		wantMail string
	}{
		{"space form", []string{"--name", "build-box", "--contact-email", "ops@example.com"}, "build-box", "ops@example.com"},
		{"equals form", []string{"--name=box2", "--contact-email=ops2@example.com"}, "box2", "ops2@example.com"},
		{"omitted", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testConfigHome(t)
			fake := &fakeSignupAPI{accountID: "AGNT-2", apiKey: "tsk_k"}
			stubBillingClient(t, fake)
			_ = captureStdout(t, func() { cmdSignup(tc.args) })

			if fake.calls != 1 {
				t.Fatalf("CreateAgent calls = %d, want 1", fake.calls)
			}
			if fake.name != tc.wantName || fake.contactEmail != tc.wantMail {
				t.Fatalf("CreateAgent(%q, %q), want (%q, %q)", fake.name, fake.contactEmail, tc.wantName, tc.wantMail)
			}
		})
	}
}

// TestSignupRequestBodyShape pins what moneyd receives: POST /agents with
// name/contact_email only when set, and no Authorization or cookie.
func TestSignupRequestBodyShape(t *testing.T) {
	testConfigHome(t)
	var paths, auths, cookies []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		cookie := ""
		if c, err := r.Cookie(sessionCookieName); err == nil {
			cookie = c.Value
		}
		cookies = append(cookies, cookie)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"account_id":"AGNT-7","api_key":"tsk_http_secret"}`)
	}))
	defer srv.Close()

	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: srv.URL}
	result, err := createAgentAccount(newBillingAPI(cfg), cfg, "build-box", "ops@example.com")
	if err != nil {
		t.Fatalf("createAgentAccount: %v", err)
	}
	if result.AccountID != "AGNT-7" || result.APIKey != "tsk_http_secret" {
		t.Fatalf("result = %+v, want the parsed account_id and api_key", result)
	}

	cfg2 := &Config{IndexerURL: defaultIndexer, BillingAPIURL: srv.URL}
	if _, err := createAgentAccount(newBillingAPI(cfg2), cfg2, "", ""); err != nil {
		t.Fatalf("createAgentAccount(no optional fields): %v", err)
	}

	if len(paths) != 2 || paths[0] != "/agents" || paths[1] != "/agents" {
		t.Fatalf("paths = %v, want POST /agents twice", paths)
	}
	if bodies[0]["name"] != "build-box" || bodies[0]["contact_email"] != "ops@example.com" {
		t.Errorf("body = %#v, want name and contact_email", bodies[0])
	}
	if len(bodies[1]) != 0 {
		t.Errorf("body with no optional fields = %#v, want {}", bodies[1])
	}
	if auths[0] != "" || cookies[0] != "" {
		t.Errorf("signup sent auth %q / cookie %q, want none (public endpoint)", auths[0], cookies[0])
	}
}

// TestCmdSignupKeyPrintedOnceAndNeverLater: the key is shown at signup only;
// later commands read the same config and must stay silent about it.
func TestCmdSignupKeyPrintedOnceAndNeverLater(t *testing.T) {
	pinStdoutTerminal(t, true)
	const key = "tsk_once_shown_secret"
	testConfigHome(t)
	fake := &fakeSignupAPI{accountID: "AGNT-3", apiKey: key}
	stubBillingClient(t, fake)

	out := captureStdout(t, func() { cmdSignup(nil) })
	if got := strings.Count(out, key); got != 1 {
		t.Fatalf("signup printed the key %d times, want exactly once:\n%s", got, out)
	}
	for _, args := range [][]string{nil, {"--json"}} {
		later := captureStdout(t, func() { cmdConfig(args) })
		if strings.Contains(later, key) {
			t.Fatalf("config %v echoed the key:\n%s", args, later)
		}
	}
}

// TestCmdSignupJSON pins the documented one-time exception: --json emits the
// account id and the key, and nothing else.
func TestCmdSignupJSON(t *testing.T) {
	testConfigHome(t)
	fake := &fakeSignupAPI{accountID: "AGNT-9", apiKey: "tsk_json_secret"}
	stubBillingClient(t, fake)

	out := captureStdout(t, func() { cmdSignup([]string{"--json"}) })
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	// v1.11.0: signup also logs in, so the one-time JSON carries the phrase
	// and the new App ID alongside the account id and key.
	if m["account_id"] != "AGNT-9" || m["api_key"] != "tsk_json_secret" || m["app_id"] != "app-AGNT-9" || m["logged_in"] != true {
		t.Fatalf("--json = %#v", m)
	}
	if p, _ := m["recovery_phrase"].(string); len(strings.Fields(p)) != 12 {
		t.Fatalf("recovery_phrase missing or not 12 words: %#v", m["recovery_phrase"])
	}
}

// TestSignupErrorMapping drives the real client against both moneyd error
// shapes: the command must surface the typed billingError and persist nothing.
func TestSignupErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
	}{
		{"nested detail.error", http.StatusUnprocessableEntity, `{"detail":{"error":"invalid_contact_email"}}`, "invalid_contact_email"},
		{"flat error", http.StatusServiceUnavailable, `{"error":"agents_unavailable"}`, "agents_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := testConfigHome(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			t.Setenv("TESSERA_BILLING_URL", srv.URL)

			result, err := signup([]string{"--name", "x"}, false)
			var be *billingError
			if !errors.As(err, &be) {
				t.Fatalf("signup error = %v (%T), want *billingError", err, err)
			}
			if be.Status != tc.status || be.Code != tc.wantCode {
				t.Fatalf("billingError = {Status:%d Code:%q}, want {%d %q}", be.Status, be.Code, tc.status, tc.wantCode)
			}
			if result.APIKey != "" {
				t.Errorf("failed signup returned a key: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(home, configFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("failed signup wrote a config (stat err=%v)", err)
			}
		})
	}
}

// TestSignupSurfacesBillingError checks a typed error from the client is
// returned unchanged; parsing nested/flat is covered in billing_test.go.
func TestSignupSurfacesBillingError(t *testing.T) {
	home := testConfigHome(t)
	want := &billingError{Status: http.StatusConflict, Code: "email_in_use", Extra: map[string]any{"hint": "try another"}}
	fake := &fakeSignupAPI{err: want}
	stubBillingClient(t, fake)

	result, err := signup(nil, false)
	if !errors.Is(err, want) {
		t.Fatalf("signup error = %v, want the client's billingError", err)
	}
	if result.APIKey != "" {
		t.Errorf("failed signup returned a key: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(home, configFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed signup wrote a config (stat err=%v)", err)
	}
}

// TestSignupSaveFailureKeepsTheKey: the server shows the key exactly once, so
// a save failure must still hand it back for printing rather than lose it.
func TestSignupSaveFailureKeepsTheKey(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESSERA_HOME", blocker)

	fake := &fakeSignupAPI{accountID: "AGNT-5", apiKey: "tsk_unsaved_secret"}
	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL}
	result, err := createAgentAccount(fake, cfg, "", "")
	if err == nil {
		t.Fatal("want a save error when the config path cannot be created")
	}
	if result.AccountID != "AGNT-5" || result.APIKey != "tsk_unsaved_secret" {
		t.Fatalf("result = %+v, want the created key returned for printing", result)
	}
}

// TestSignupCorruptConfigIsNotClobbered: a config that exists but cannot be
// read must not be silently replaced by a fresh one.
func TestSignupCorruptConfigIsNotClobbered(t *testing.T) {
	home := testConfigHome(t)
	cp := filepath.Join(home, configFileName)
	if err := os.WriteFile(cp, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSignupAPI{accountID: "A", apiKey: "tsk_k"}
	stubBillingClient(t, fake)

	result, err := signup(nil, false)
	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("signup error = %v, want a config read error", err)
	}
	if result.APIKey != "" {
		t.Errorf("failed signup returned a key: %+v", result)
	}
	if fake.calls != 0 {
		t.Errorf("CreateAgent ran %d time(s) despite an unreadable config", fake.calls)
	}
	data, rerr := os.ReadFile(cp)
	if rerr != nil || string(data) != "{not json" {
		t.Fatalf("corrupt config was overwritten: %q (err=%v)", data, rerr)
	}
}

// TestCmdSignupPreservesExistingConfig: signing up from a legacy config keeps
// its non-credential settings; the stale storage identity is replaced by the
// new account's own (v1.11.0 auto-login), never carried over.
func TestCmdSignupPreservesExistingConfig(t *testing.T) {
	home := testConfigHome(t)
	cp := filepath.Join(home, configFileName)
	legacy := `{"indexer_url":"` + defaultIndexer + `","app_id":"app-123","default_jobs":4}`
	if err := os.WriteFile(cp, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSignupAPI{accountID: "AGNT-4", apiKey: "tsk_keep"}
	stubBillingClient(t, fake)

	_ = captureStdout(t, func() { cmdSignup(nil) })

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AppID != "app-AGNT-4" || cfg.DefaultJobs != 4 {
		t.Errorf("existing settings were lost: AppID=%q DefaultJobs=%d", cfg.AppID, cfg.DefaultJobs)
	}
	if cfg.BillingAPIURL != defaultBillingURL {
		t.Errorf("BillingAPIURL = %q, want the default filled in", cfg.BillingAPIURL)
	}
	if cfg.BillingAgentKey != "tsk_keep" {
		t.Errorf("BillingAgentKey = %q, want the returned key", cfg.BillingAgentKey)
	}
}

// TestSignupGuardAgainstOverwrite: a second signup without --force must not
// create another account or touch the stored key; --force overwrites it and
// prints the new key once.
func TestSignupGuardAgainstOverwrite(t *testing.T) {
	pinStdoutTerminal(t, true)
	const overwriteMsg = "this machine is already logged in to a Tessera account; pass --force to create a new account (the old one stays, log back in with its recovery phrase)"
	testConfigHome(t)
	if err := saveConfig(&Config{IndexerURL: defaultIndexer, AppID: "app-1", BillingAgentKey: "tsk_old_key"}); err != nil {
		t.Fatal(err)
	}

	t.Run("no force keeps the old key", func(t *testing.T) {
		fake := &fakeSignupAPI{accountID: "AGNT-NEW", apiKey: "tsk_new_key"}
		stubBillingClient(t, fake)

		result, err := signup(nil, false)
		if !errors.Is(err, errSignupKeyExists) {
			t.Fatalf("signup error = %v (%T), want errSignupKeyExists", err, err)
		}
		if err.Error() != overwriteMsg {
			t.Fatalf("error = %q, want %q", err.Error(), overwriteMsg)
		}
		if fake.calls != 0 {
			t.Fatalf("CreateAgent calls = %d, want 0", fake.calls)
		}
		if result.APIKey != "" {
			t.Errorf("failed signup returned a key: %+v", result)
		}
		cfg, lerr := loadConfig()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if cfg.BillingAgentKey != "tsk_old_key" {
			t.Fatalf("stored key = %q, want the old key untouched", cfg.BillingAgentKey)
		}
	})

	t.Run("force proceeds and overwrites", func(t *testing.T) {
		fake := &fakeSignupAPI{accountID: "AGNT-NEW", apiKey: "tsk_new_key"}
		stubBillingClient(t, fake)

		out := captureStdout(t, func() { cmdSignup([]string{"--force"}) })
		if fake.calls != 1 {
			t.Fatalf("CreateAgent calls = %d, want 1 with --force", fake.calls)
		}
		if got := strings.Count(out, "tsk_new_key"); got != 1 {
			t.Fatalf("--force printed the new key %d times, want exactly once:\n%s", got, out)
		}
		cfg, lerr := loadConfig()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if cfg.BillingAgentKey != "tsk_new_key" {
			t.Fatalf("stored key = %q, want the new key after --force", cfg.BillingAgentKey)
		}
	})
}

func pinStdoutTerminal(t *testing.T, tty bool) {
	t.Helper()
	old := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return tty }
	t.Cleanup(func() { stdoutIsTerminal = old })
}

// TT retest 3 #8: piped signup output never contains the secrets; they land
// in a 0600 file whose path is printed, so the phrase is not lost.
func TestSignupNonTTYWritesSecretsFile(t *testing.T) {
	home := testConfigHome(t)
	pinStdoutTerminal(t, false)
	res := signupResult{AccountID: "AGNT-x1", APIKey: "tsk_piped-Secret", Phrase: "alpha beta gamma", AppID: "app1"}
	out := captureStdout(t, func() { printSignupResultTo(res, signupOutputMode(nil, false)) })
	if strings.Contains(out, "tsk_piped-Secret") || strings.Contains(out, "alpha beta gamma") {
		t.Fatalf("secrets printed to a non-terminal:\n%s", out)
	}
	p, _ := defaultSecretsPath("AGNT-x1")
	if !strings.HasPrefix(p, home) || !strings.Contains(out, p) {
		t.Fatalf("path %q not under %q or not printed:\n%s", p, home, out)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file mode: %v %v", st, err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "recovery_phrase=alpha beta gamma") || !strings.Contains(string(data), "api_key=tsk_piped-Secret") {
		t.Fatalf("secrets file content incomplete")
	}
	// --show-secrets restores printing.
	out = captureStdout(t, func() { printSignupResultTo(res, signupOutputMode([]string{"--show-secrets"}, false)) })
	if strings.Count(out, "tsk_piped-Secret") != 1 {
		t.Fatalf("--show-secrets did not print the key once")
	}
	// --json --secrets-file omits the secrets from JSON.
	sf := filepath.Join(t.TempDir(), "s.txt")
	out = captureStdout(t, func() { printSignupResultTo(res, signupOutputMode([]string{"--secrets-file", sf}, true)) })
	if strings.Contains(out, "tsk_piped") || !strings.Contains(out, `"secrets_file"`) {
		t.Fatalf("json with secrets file: %s", out)
	}
	// plain --json keeps the explicit fields.
	out = captureStdout(t, func() { printSignupResultTo(res, signupOutputMode(nil, true)) })
	if !strings.Contains(out, "tsk_piped-Secret") {
		t.Fatalf("--json lost api_key")
	}
}
