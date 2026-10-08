package main

import (
	"bytes"
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

// fakeLoginAPI implements billingAPI for the billing login flows. The embedded
// nil interface satisfies every other method and panics if a flow calls one,
// which is itself part of the assertion: login only speaks magic-link, verify,
// account and logout.
type fakeLoginAPI struct {
	billingAPI
	magicCalls  int
	magicEmail  string
	magicErr    error
	verifyCalls int
	verifyToken string
	session     string
	verifyErr   error
	account     map[string]any
	accountErr  error
	logoutCalls int
	logoutSess  string
	logoutErr   error
}

func (f *fakeLoginAPI) MagicLink(email string) error {
	f.magicCalls++
	f.magicEmail = email
	return f.magicErr
}

func (f *fakeLoginAPI) Verify(token string) (string, error) {
	f.verifyCalls++
	f.verifyToken = token
	return f.session, f.verifyErr
}

func (f *fakeLoginAPI) Account() (map[string]any, error) {
	return f.account, f.accountErr
}

func (f *fakeLoginAPI) Logout(session string) error {
	f.logoutCalls++
	f.logoutSess = session
	return f.logoutErr
}

// TestLoginValueFlagsRegistered guards the positional() trap the plan calls
// out: --email and --token take separate values, so takesValue must know them.
// --billing-logout is a boolean and must not consume the next argument.
func TestLoginValueFlagsRegistered(t *testing.T) {
	for _, flag := range []string{"--email", "--token"} {
		if !takesValue(flag) {
			t.Errorf("takesValue(%q) = false; positional() would swallow its value", flag)
		}
	}
	if takesValue("--billing-logout") {
		t.Error("takesValue(--billing-logout) = true; it is a boolean and must not consume a value")
	}
}

// TestBillingLoginFlags pins the dispatch decision: no billing flag means the
// legacy flow; --billing-logout wins; malformed pairs are rejected instead of
// silently falling through to the interactive legacy path.
func TestBillingLoginFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		email   string
		token   string
		logout  bool
		billing bool
		wantErr bool
	}{
		{"no flags is the legacy flow", nil, "", "", false, false, false},
		{"phrase positional is the legacy flow", []string{"word1 word2"}, "", "", false, false, false},
		{"email only", []string{"--email", "u@example.com"}, "u@example.com", "", false, true, false},
		{"space form", []string{"--email", "u@example.com", "--token", "tok"}, "u@example.com", "tok", false, true, false},
		{"equals form", []string{"--email=u@example.com", "--token=tok"}, "u@example.com", "tok", false, true, false},
		{"billing logout", []string{"--billing-logout"}, "", "", true, true, false},
		{"billing logout wins", []string{"--email", "u@example.com", "--billing-logout"}, "", "", true, true, false},
		{"email without value", []string{"--email"}, "", "", false, true, true},
		{"empty email", []string{"--email="}, "", "", false, true, true},
		{"token without value", []string{"--email", "u@e", "--token"}, "", "", false, true, true},
		{"token without email", []string{"--token", "tok"}, "", "", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email, token, logout, billing, err := billingLoginFlags(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("billingLoginFlags(%v) = no error, want one", tc.args)
				}
				if !billing {
					t.Fatalf("billingLoginFlags(%v) reported the legacy flow for a billing flag", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("billingLoginFlags(%v): %v", tc.args, err)
			}
			if email != tc.email || token != tc.token || logout != tc.logout || billing != tc.billing {
				t.Fatalf("got (email=%q token=%q logout=%v billing=%v), want (%q %q %v %v)",
					email, token, logout, billing, tc.email, tc.token, tc.logout, tc.billing)
			}
		})
	}
}

// TestCmdLoginEmailOnlyStoresNothing: `login --email` requests the link, prints
// the instruction, creates/modifies no config, and — because the dispatch runs
// before the legacy flow — never shows the already-logged-in prompt.
func TestCmdLoginEmailOnlyStoresNothing(t *testing.T) {
	home := testConfigHome(t)
	cfg := &Config{
		IndexerURL:    defaultIndexer,
		BillingAPIURL: defaultBillingURL,
		AppID:         "app-1",
		AppKey:        strings.Repeat("ab", 32),
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(home, configFileName)
	before, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeLoginAPI{}
	stubBillingClient(t, fake)

	out := captureStdout(t, func() { cmdLogin([]string{"--email", "user@example.com"}) })

	if fake.magicCalls != 1 || fake.magicEmail != "user@example.com" {
		t.Fatalf("MagicLink calls = %d (email %q), want one with user@example.com", fake.magicCalls, fake.magicEmail)
	}
	if fake.verifyCalls != 0 {
		t.Fatalf("Verify calls = %d, want 0 without --token", fake.verifyCalls)
	}
	if !strings.Contains(out, "Check your inbox") {
		t.Errorf("instruction missing from output:\n%s", out)
	}
	if !strings.Contains(out, "tessera login --email user@example.com --token") {
		t.Errorf("follow-up command missing from output:\n%s", out)
	}
	if strings.Contains(out, "Already logged in") {
		t.Errorf("the legacy already-logged-in prompt ran for --email:\n%s", out)
	}
	after, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("--email changed the config:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestBillingLoginEmailTokenStoresSessionAndValidates drives the real HTTP
// client against a fake moneyd: it must skip the magic-link POST (avoiding the
// per-email cooldown), capture the Set-Cookie session, persist it, prove it
// with GET /account using the cookie (not the stored agent key), and print the
// account summary while leaving the agent key untouched.
func TestBillingLoginEmailTokenStoresSessionAndValidates(t *testing.T) {
	testConfigHome(t)
	var paths []string
	var verifyToken, accountCookie, accountAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/verify":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			verifyToken = body["token"]
			http.SetCookie(w, &http.Cookie{
				Name: sessionCookieName, Value: "sess-captured", Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			fmt.Fprint(w, `{}`)
		case "/account":
			if c, err := r.Cookie(sessionCookieName); err == nil {
				accountCookie = c.Value
			}
			accountAuth = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"billing_id":"TESS-42","kind":"human","status":"active","balance_usd":"7.50","plan":"payg","days_left":29}`)
		default:
			t.Errorf("unexpected request path %s (login must not POST /auth/magic-link with --token)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   srv.URL,
		AppID:           "app-1",
		AppKey:          strings.Repeat("ab", 32),
		BillingAgentKey: "tsk_keep",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("user@example.com", "tok-1") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}

	if len(paths) != 2 || paths[0] != "/auth/verify" || paths[1] != "/account" {
		t.Fatalf("paths = %v, want [/auth/verify /account]", paths)
	}
	if verifyToken != "tok-1" {
		t.Fatalf("verify token = %q, want tok-1", verifyToken)
	}
	if accountCookie != "sess-captured" {
		t.Fatalf("account cookie = %q, want the captured session", accountCookie)
	}
	if accountAuth != "" {
		t.Fatalf("account Authorization = %q, want none (session auth despite the stored agent key)", accountAuth)
	}

	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.BillingSession != "sess-captured" {
		t.Fatalf("BillingSession = %q, want the Set-Cookie value", saved.BillingSession)
	}
	if saved.BillingAgentKey != "tsk_keep" || saved.AppKey != cfg.AppKey {
		t.Fatalf("login touched other credentials: agent key=%q app key=%q", saved.BillingAgentKey, saved.AppKey)
	}
	cp, err := configFilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"billing_session": "sess-captured"`) {
		t.Errorf("session not persisted in the config:\n%s", data)
	}
	if !strings.Contains(out, "TESS-42") || !strings.Contains(out, "$7.50") {
		t.Errorf("account summary missing from output:\n%s", out)
	}
}

// TestBillingLoginSessionExpiryClearsAndGuides: after a successful verify, a
// 401 on GET /account is the client's session-expiry signal — the command
// surfaces its login guidance and the dead session is gone from the config.
func TestBillingLoginSessionExpiryClearsAndGuides(t *testing.T) {
	testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/verify":
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "sess-dead", Path: "/"})
			fmt.Fprint(w, `{}`)
		case "/account":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"detail":{"error":"unauthorized"}}`)
		}
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL)

	err := billingLogin("user@example.com", "tok")
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("billingLogin error = %v (%T), want errSessionExpired", err, err)
	}
	if !strings.Contains(err.Error(), "tessera login --email") {
		t.Fatalf("error = %q, want the re-login guidance", err)
	}
	saved, lerr := loadConfig()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if saved.BillingSession != "" {
		t.Fatalf("dead session was not cleared: %q", saved.BillingSession)
	}
}

// TestBillingLoginInvalidTokenGuidance: verify's flat 401 invalid_token must
// come back actionable (request a fresh link), not as a bare API error, and
// nothing is persisted.
func TestBillingLoginInvalidTokenGuidance(t *testing.T) {
	home := testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_token"}`)
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL)

	err := billingLogin("user@example.com", "stale-token")
	if err == nil {
		t.Fatal("billingLogin with an invalid token = nil, want an error")
	}
	if !strings.Contains(err.Error(), "invalid") || !strings.Contains(err.Error(), "tessera login --email") {
		t.Fatalf("error = %q, want actionable invalid-link guidance", err)
	}
	var be *billingError
	if !errors.As(err, &be) || be.Code != "invalid_token" {
		t.Fatalf("wrapped error lost the billingError: %v (%T)", err, err)
	}
	if _, serr := os.Stat(filepath.Join(home, configFileName)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("invalid_token wrote a config (stat err=%v)", serr)
	}
}

// TestBillingLogoutRevokesAndClears is the real-client end-to-end: the stored
// session is sent as the cookie on POST /auth/logout, then cleared locally.
// The agent key and app key must survive.
func TestBillingLogoutRevokesAndClears(t *testing.T) {
	testConfigHome(t)
	var logoutPath, logoutMethod, logoutCookie, logoutAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logoutPath, logoutMethod = r.URL.Path, r.Method
		if c, err := r.Cookie(sessionCookieName); err == nil {
			logoutCookie = c.Value
		}
		logoutAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   srv.URL,
		AppID:           "app-1",
		AppKey:          strings.Repeat("ab", 32),
		BillingSession:  "sess-live",
		BillingAgentKey: "tsk_keep",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	var logoutErr error
	out := captureStdout(t, func() { logoutErr = billingLogout() })
	if logoutErr != nil {
		t.Fatalf("billingLogout: %v", logoutErr)
	}

	if logoutPath != "/auth/logout" || logoutMethod != http.MethodPost {
		t.Fatalf("logout request = %s %s, want POST /auth/logout", logoutMethod, logoutPath)
	}
	if logoutCookie != "sess-live" {
		t.Fatalf("logout cookie = %q, want the stored session", logoutCookie)
	}
	if logoutAuth != "" {
		t.Fatalf("logout Authorization = %q, want none (cookie auth only)", logoutAuth)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.BillingSession != "" {
		t.Fatalf("BillingSession = %q, want it cleared", saved.BillingSession)
	}
	if saved.BillingAgentKey != "tsk_keep" || saved.AppKey != cfg.AppKey {
		t.Fatalf("logout touched other credentials: agent key=%q app key=%q", saved.BillingAgentKey, saved.AppKey)
	}
	if !strings.Contains(out, "Signed out") {
		t.Errorf("confirmation missing from output:\n%s", out)
	}
}

// TestBillingLogoutBestEffortWhenServerFails: an unreachable or already-dead
// session must not leave the local copy behind, and the command still exits
// successfully. This also drives `login --billing-logout` through the dispatch.
func TestBillingLogoutBestEffortWhenServerFails(t *testing.T) {
	testConfigHome(t)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingSession:  "sess-dead",
		BillingAgentKey: "tsk_keep",
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLoginAPI{logoutErr: errors.New("connection refused")}
	stubBillingClient(t, fake)

	out := captureStdout(t, func() { cmdLogin([]string{"--billing-logout"}) })

	if fake.logoutCalls != 1 || fake.logoutSess != "sess-dead" {
		t.Fatalf("Logout calls = %d (session %q), want one with sess-dead", fake.logoutCalls, fake.logoutSess)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.BillingSession != "" {
		t.Fatalf("BillingSession = %q, want it cleared despite the revoke failure", saved.BillingSession)
	}
	if saved.BillingAgentKey != "tsk_keep" {
		t.Fatalf("agent key = %q, want it untouched", saved.BillingAgentKey)
	}
	if !strings.Contains(out, "Signed out") {
		t.Errorf("confirmation missing from output:\n%s", out)
	}
}

// TestBillingLogoutWithoutSession: nothing stored means nothing revoked and no
// config invented.
func TestBillingLogoutWithoutSession(t *testing.T) {
	home := testConfigHome(t)
	fake := &fakeLoginAPI{}
	stubBillingClient(t, fake)

	var logoutErr error
	out := captureStdout(t, func() { logoutErr = billingLogout() })
	if logoutErr != nil {
		t.Fatalf("billingLogout: %v", logoutErr)
	}
	if fake.logoutCalls != 0 {
		t.Fatalf("Logout calls = %d, want 0 with no stored session", fake.logoutCalls)
	}
	if !strings.Contains(out, "No billing session") {
		t.Errorf("output = %q, want the nothing-to-do notice", out)
	}
	if _, err := os.Stat(filepath.Join(home, configFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("logout wrote a config (stat err=%v)", err)
	}
}
