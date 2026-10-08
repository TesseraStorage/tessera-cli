package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sia.tech/core/types"
)

// fakeLoginAPI implements billingAPI for the billing login flows. The embedded
// nil interface satisfies every other method and panics if a flow calls one,
// which is itself part of the assertion: login only speaks magic-link, verify,
// account, logout, and the registration pair credentials/activate.
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

	credentialsCalls int
	connectKey       string
	credentialsErr   error
	activateCalls    int
	activateErr      error
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

func (f *fakeLoginAPI) Credentials() (string, error) {
	f.credentialsCalls++
	return f.connectKey, f.credentialsErr
}

func (f *fakeLoginAPI) Activate() error {
	f.activateCalls++
	return f.activateErr
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
			fmt.Fprint(w, `{"billing_id":"TESS-42","kind":"human","status":"active","balance_usd":7.5,"plan":"payg","days_left":29}`)
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
	out := captureStdout(t, func() { loginErr = billingLogin("user@example.com", "tok-1", "") })
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

	err := billingLogin("user@example.com", "tok", "")
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

	err := billingLogin("user@example.com", "stale-token", "")
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

// --- headless registration after billing auth -----------------------------

// fakeRegisteredSDK is the registeredApp a fake builder hands back.
type fakeRegisteredSDK struct {
	key    types.PrivateKey
	closed bool
}

func (f *fakeRegisteredSDK) AppKey() types.PrivateKey { return f.key }
func (f *fakeRegisteredSDK) Close() error             { f.closed = true; return nil }

// fakeRegistrationBuilder implements registrationBuilder, capturing the SDK
// calls the headless flow makes.
type fakeRegistrationBuilder struct {
	appID         types.Hash256
	respURL       string
	requestCalls  int
	requestErr    error
	waitCalls     int
	waitErr       error
	registerCalls int
	registerErr   error
	phrase        string
	app           registeredApp
}

func (f *fakeRegistrationBuilder) RequestConnection(context.Context) (string, error) {
	f.requestCalls++
	return f.respURL, f.requestErr
}

func (f *fakeRegistrationBuilder) WaitForApproval(context.Context) error {
	f.waitCalls++
	return f.waitErr
}

func (f *fakeRegistrationBuilder) Register(_ context.Context, mnemonic string) (registeredApp, error) {
	f.registerCalls++
	f.phrase = mnemonic
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return f.app, nil
}

// stubSDKBuilder swaps the builder seam for fake and records the app ID the
// flow chose.
func stubSDKBuilder(t *testing.T, fake *fakeRegistrationBuilder) {
	t.Helper()
	orig := newSDKBuilder
	newSDKBuilder = func(_ string, appID types.Hash256) registrationBuilder {
		fake.appID = appID
		return fake
	}
	t.Cleanup(func() { newSDKBuilder = orig })
}

type approveCall struct {
	responseURL string
	connectKey  string
}

type approveRecorder struct {
	calls []approveCall
}

// stubApproveConnectURL swaps the approval seam and records each call.
func stubApproveConnectURL(t *testing.T, rec *approveRecorder, err error) {
	t.Helper()
	orig := approveConnectURL
	approveConnectURL = func(_ context.Context, responseURL, connectKey string) error {
		rec.calls = append(rec.calls, approveCall{responseURL, connectKey})
		return err
	}
	t.Cleanup(func() { approveConnectURL = orig })
}

// stubStdinTerminal pins the interactive-stdin gate so the prompt assertions do
// not depend on how the test binary's stdin is wired.
func stubStdinTerminal(t *testing.T, terminal bool) {
	t.Helper()
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return terminal }
	t.Cleanup(func() { stdinIsTerminal = orig })
}

// stubStdin replaces os.Stdin with a pipe holding input, so the optional
// master-password prompt can be driven without a real terminal.
func stubStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
}

// stubHeadlessRegistration installs a fake billing client and a fake builder
// around the registration flow. stdin is pinned to "not a terminal" and the
// approval is stubbed; tests assert on the returned builder and recorder.
func stubHeadlessRegistration(t *testing.T, fake *fakeLoginAPI) (*fakeRegistrationBuilder, *approveRecorder) {
	t.Helper()
	stubStdinTerminal(t, false)
	stubBillingClient(t, fake)
	builder := &fakeRegistrationBuilder{
		respURL: "https://index.example/auth/connect/req-1",
		app:     &fakeRegisteredSDK{key: types.GeneratePrivateKey()},
	}
	stubSDKBuilder(t, builder)
	rec := &approveRecorder{}
	stubApproveConnectURL(t, rec, nil)
	return builder, rec
}

// TestApproveConnectURL is the exact wire contract for the headless browser
// step: POST to the SDK's response URL, connect key as the Basic password
// (empty username), body {"approve":true}, and the key never echoed in errors.
func TestApproveConnectURL(t *testing.T) {
	var method, path, user, pass, body, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		user, pass, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := approveConnectURL(context.Background(), srv.URL+"/auth/connect/req-7", "ck_secret"); err != nil {
		t.Fatalf("approveConnectURL: %v", err)
	}
	if method != http.MethodPost || path != "/auth/connect/req-7" {
		t.Fatalf("request = %s %s, want POST /auth/connect/req-7", method, path)
	}
	if user != "" || pass != "ck_secret" {
		t.Fatalf("Basic auth = (%q, %q), want (\"\", the connect key)", user, pass)
	}
	if body != `{"approve":true}` {
		t.Fatalf("body = %q, want the approve payload", body)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}

	t.Run("non-2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusBadRequest)
		}))
		defer srv.Close()
		err := approveConnectURL(context.Background(), srv.URL+"/auth/connect/req-8", "ck_secret")
		if err == nil {
			t.Fatal("approveConnectURL on a 400 = nil, want an error")
		}
		if !strings.Contains(err.Error(), "400") {
			t.Errorf("error = %q, want the status", err)
		}
		if strings.Contains(err.Error(), "ck_secret") {
			t.Errorf("error leaked the connect key: %v", err)
		}
	})
}

// TestBillingLoginRegistersHeadless drives the whole human path against a fake
// moneyd: verify → account → credentials (session cookie, never the stored
// agent key) → SDK connection → headless approval → register with a fresh
// phrase → persist app_id/app_key → activate (after the config was saved).
// The connect key and session must never reach stdout.
func TestBillingLoginRegistersHeadless(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	stubStdinTerminal(t, false)

	var paths, auths, cookies []string
	var activateSawSaved bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		if c, err := r.Cookie(sessionCookieName); err == nil {
			cookies = append(cookies, c.Value)
		} else {
			cookies = append(cookies, "")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/verify":
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "sess-captured", Path: "/"})
			fmt.Fprint(w, `{}`)
		case "/account":
			fmt.Fprint(w, `{"billing_id":"TESS-42","kind":"human","status":"active","balance_usd":"7.50","plan":"payg","days_left":29}`)
		case "/credentials":
			fmt.Fprint(w, `{"connect_key":"ck_secret","quota":"plan-pending"}`)
		case "/credentials/activate":
			// Activation is step 5: the app credentials must already be on
			// disk when moneyd links the identity (spec §6.3.6).
			if saved, err := loadConfig(); err == nil && saved.AppID != "" && saved.AppKey != "" {
				activateSawSaved = true
			}
			fmt.Fprint(w, `{"linked":true}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL)

	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_agent",
	}); err != nil {
		t.Fatal(err)
	}

	key := types.GeneratePrivateKey()
	builder := &fakeRegistrationBuilder{
		respURL: "http://index.example/auth/connect/req-1",
		app:     &fakeRegisteredSDK{key: key},
	}
	stubSDKBuilder(t, builder)
	var rec approveRecorder
	stubApproveConnectURL(t, &rec, nil)

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("user@example.com", "tok-1", "") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}

	wantPaths := []string{"/auth/verify", "/account", "/credentials", "/credentials/activate"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
	for i, p := range paths {
		if p != "/credentials" && p != "/credentials/activate" {
			continue
		}
		if auths[i] != "" {
			t.Errorf("%s Authorization = %q, want none: the just-verified session wins over the stored agent key", p, auths[i])
		}
		if cookies[i] != "sess-captured" {
			t.Errorf("%s cookie = %q, want the just-verified session", p, cookies[i])
		}
	}
	if !activateSawSaved {
		t.Error("activate ran before app_id/app_key were persisted")
	}
	if builder.requestCalls != 1 || builder.waitCalls != 1 || builder.registerCalls != 1 {
		t.Fatalf("builder calls = request %d, wait %d, register %d; want 1/1/1",
			builder.requestCalls, builder.waitCalls, builder.registerCalls)
	}
	if sdk, ok := builder.app.(*fakeRegisteredSDK); !ok || !sdk.closed {
		t.Error("the registered SDK was not closed")
	}
	if len(rec.calls) != 1 || rec.calls[0].responseURL != builder.respURL || rec.calls[0].connectKey != "ck_secret" {
		t.Fatalf("approvals = %+v, want one for %s with the minted connect key", rec.calls, builder.respURL)
	}
	if len(strings.Fields(builder.phrase)) != 12 {
		t.Fatalf("register phrase = %q, want a generated 12-word phrase", builder.phrase)
	}
	if n := strings.Count(out, builder.phrase); n != 1 {
		t.Errorf("generated phrase appears %d times in output, want exactly once:\n%s", n, out)
	}
	if n := strings.Count(out, "RECOVERY PHRASE — SAVE THESE 12 WORDS"); n != 1 {
		t.Errorf("recovery banner appears %d times, want exactly once", n)
	}
	for _, secret := range []string{"ck_secret", "sess-captured", "tsk_agent"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaked %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "Storage identity linked") {
		t.Errorf("final activation state missing from output:\n%s", out)
	}

	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppID == "" || saved.AppID != builder.appID.String() {
		t.Fatalf("saved AppID = %q, builder got %q", saved.AppID, builder.appID.String())
	}
	if want := hex.EncodeToString([]byte(key)); saved.AppKey != want {
		t.Fatalf("saved AppKey = %q, want the registered key %q", saved.AppKey, want)
	}
	if saved.BillingSession != "sess-captured" || saved.BillingAgentKey != "tsk_agent" {
		t.Fatalf("billing credentials changed: session %q agent key %q", saved.BillingSession, saved.BillingAgentKey)
	}
	if saved.PhraseEncrypted != "" || saved.PhraseSalt != "" {
		t.Error("phrase was encrypted although stdin is not a terminal")
	}
}

// TestBillingLoginHonorsEnvPhrase: a caller-supplied TESSERA_PHRASE is used
// verbatim (like the legacy flow) and is not echoed — only the app key is
// persisted.
func TestBillingLoginHonorsEnvPhrase(t *testing.T) {
	testConfigHome(t)
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	t.Setenv("TESSERA_PHRASE", phrase)
	fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{"billing_id": "TESS-1"}}
	builder, _ := stubHeadlessRegistration(t, fake)

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}
	if builder.phrase != phrase {
		t.Fatalf("register phrase = %q, want the TESSERA_PHRASE value", builder.phrase)
	}
	if strings.Contains(out, phrase) {
		t.Errorf("a supplied phrase must not be echoed:\n%s", out)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppKey == "" || saved.AppID == "" {
		t.Fatal("app credentials were not persisted")
	}
}

// TestCmdLoginEmailTokenUsesPositionalPhrase: `login --email <e> --token <t>
// "<words>"` must use the supplied positional phrase for the headless storage
// registration (the README promise), exactly like TESSERA_PHRASE — not silently
// ignore it and mint a fresh random phrase.
func TestCmdLoginEmailTokenUsesPositionalPhrase(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{"billing_id": "TESS-1"}}
	builder, _ := stubHeadlessRegistration(t, fake)

	out := captureStdout(t, func() {
		cmdLogin([]string{"--email", "u@example.com", "--token", "tok", phrase})
	})

	if builder.registerCalls != 1 {
		t.Fatalf("Register calls = %d, want 1", builder.registerCalls)
	}
	if builder.phrase != phrase {
		t.Fatalf("register phrase = %q, want the supplied positional phrase", builder.phrase)
	}
	if strings.Contains(out, "RECOVERY PHRASE — SAVE THESE 12 WORDS") {
		t.Errorf("a supplied phrase must not trigger the fresh-phrase banner:\n%s", out)
	}
	if !strings.Contains(out, "Using the recovery phrase you supplied") {
		t.Errorf("supplied-phrase note missing from output:\n%s", out)
	}
	if strings.Contains(out, phrase) {
		t.Errorf("a supplied phrase must not be echoed:\n%s", out)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppKey == "" || saved.AppID == "" {
		t.Fatal("app credentials were not persisted")
	}
}

// TestBillingLoginSkipsRegistrationWhenRegistered: an account that already has
// app_id/app_key keeps them; the registration endpoints are never called.
func TestBillingLoginSkipsRegistrationWhenRegistered(t *testing.T) {
	testConfigHome(t)
	fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{"billing_id": "TESS-1"}}
	builder, rec := stubHeadlessRegistration(t, fake)
	if err := saveConfig(&Config{
		IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL,
		AppID: "app-1", AppKey: strings.Repeat("ab", 32),
	}); err != nil {
		t.Fatal(err)
	}

	var loginErr error
	_ = captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}
	if fake.credentialsCalls != 0 || fake.activateCalls != 0 {
		t.Fatalf("registration endpoints called for a registered account: credentials %d, activate %d",
			fake.credentialsCalls, fake.activateCalls)
	}
	if builder.requestCalls != 0 || len(rec.calls) != 0 {
		t.Fatalf("SDK registration ran for a registered account: request %d, approvals %d",
			builder.requestCalls, len(rec.calls))
	}
}

// TestBillingLoginIdentityExistsGuides: POST /credentials 409 identity_exists
// becomes recovery guidance; no builder call, no activation, no connect key.
func TestBillingLoginIdentityExistsGuides(t *testing.T) {
	testConfigHome(t)
	fake := &fakeLoginAPI{
		session:        "sess-1",
		account:        map[string]any{"billing_id": "TESS-1"},
		connectKey:     "ck_never_minted",
		credentialsErr: &billingError{Status: 409, Code: "identity_exists"},
	}
	builder, rec := stubHeadlessRegistration(t, fake)

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr == nil {
		t.Fatal("billingLogin with 409 identity_exists = nil, want guidance")
	}
	if !errors.Is(loginErr, errIdentityExists) {
		t.Fatalf("error = %v (%T), want errIdentityExists", loginErr, loginErr)
	}
	for _, want := range []string{"identity_exists", "already", "config.json", "contact support"} {
		if !strings.Contains(loginErr.Error(), want) {
			t.Errorf("guidance %q missing from error: %v", want, loginErr)
		}
	}
	if strings.Contains(loginErr.Error(), "recovery phrase") {
		t.Errorf("guidance still points at the recovery-phrase restore this CLI cannot perform: %v", loginErr)
	}
	if builder.requestCalls != 0 || len(rec.calls) != 0 {
		t.Fatalf("registration continued after 409: request %d, approvals %d", builder.requestCalls, len(rec.calls))
	}
	if fake.activateCalls != 0 {
		t.Fatalf("activate calls = %d, want 0 after 409", fake.activateCalls)
	}
	if strings.Contains(out, "ck_never_minted") {
		t.Errorf("output leaked a connect key:\n%s", out)
	}
}

// TestBillingLoginActivateAlreadyLinkedSucceeds: a reconcile tick may link the
// identity between Register and activate; the server then answers 409
// already_registered, which is the desired end state, not a failure.
func TestBillingLoginActivateAlreadyLinkedSucceeds(t *testing.T) {
	testConfigHome(t)
	fake := &fakeLoginAPI{
		session:     "sess-1",
		account:     map[string]any{"billing_id": "TESS-1"},
		connectKey:  "ck_1",
		activateErr: &billingError{Status: 409, Code: "already_registered"},
	}
	builder, _ := stubHeadlessRegistration(t, fake)

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}
	if builder.registerCalls != 1 || fake.activateCalls != 1 {
		t.Fatalf("register %d, activate %d; want 1/1", builder.registerCalls, fake.activateCalls)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppKey == "" {
		t.Error("app key was not persisted before the benign activate 409")
	}
	if !strings.Contains(out, "Storage identity linked") {
		t.Errorf("success state missing from output:\n%s", out)
	}
}

// TestBillingLoginRegisterFailureAnnotatesMintedState: once /credentials has
// minted an identity, a later failure must tell the operator the account state
// — the identity exists but was never completed — naming the billing id from
// the login flow, keeping the underlying error retrievable, printing the
// phrase before the failure, and writing no partial app credentials locally.
func TestBillingLoginRegisterFailureAnnotatesMintedState(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{"billing_id": "TESS-42"}, connectKey: "ck_secret"}
	builder, _ := stubHeadlessRegistration(t, fake)
	registerErr := errors.New("indexer unreachable")
	builder.registerErr = registerErr

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr == nil {
		t.Fatal("billingLogin with a failing Register = nil, want the annotated error")
	}
	for _, want := range []string{"registration incomplete", "TESS-42", "minted but not completed", "contact support"} {
		if !strings.Contains(loginErr.Error(), want) {
			t.Errorf("error %q missing %q", loginErr, want)
		}
	}
	if !errors.Is(loginErr, registerErr) {
		t.Errorf("error %v does not wrap the underlying failure", loginErr)
	}
	if strings.Contains(loginErr.Error(), "ck_secret") {
		t.Errorf("error leaked the connect key: %v", loginErr)
	}
	// The phrase is shown before Register, so a failed registration can never
	// lose it.
	if len(strings.Fields(builder.phrase)) != 12 || !strings.Contains(out, builder.phrase) {
		t.Errorf("generated phrase was not printed before the failure:\n%s", out)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppID != "" || saved.AppKey != "" {
		t.Fatalf("partial app credentials persisted: app_id %q app_key %q", saved.AppID, saved.AppKey)
	}
	if saved.BillingSession != "sess-1" {
		t.Fatalf("billing session = %q, want the successful login preserved", saved.BillingSession)
	}
}

// TestBillingLoginMintFailuresSkipRegister: a failure before Register
// (RequestConnection, headless approval or WaitForApproval) gets the same
// account-state annotation, never calls Register and leaves no app credentials
// behind.
func TestBillingLoginMintFailuresSkipRegister(t *testing.T) {
	cases := []struct {
		name       string
		requestErr error
		approveErr error
		waitErr    error
	}{
		{"request error", errors.New("indexer down"), nil, nil},
		{"approve error", nil, errors.New("approve rejected"), nil},
		{"wait error", nil, nil, errors.New("request expired")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testConfigHome(t)
			t.Setenv("TESSERA_PHRASE", "")
			fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{"billing_id": "TESS-42"}, connectKey: "ck_secret"}
			builder, rec := stubHeadlessRegistration(t, fake)
			builder.requestErr = tc.requestErr
			builder.waitErr = tc.waitErr
			if tc.approveErr != nil {
				stubApproveConnectURL(t, rec, tc.approveErr)
			}

			var loginErr error
			_ = captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
			if loginErr == nil {
				t.Fatal("billingLogin = nil, want the annotated failure")
			}
			if !strings.Contains(loginErr.Error(), "registration incomplete") || !strings.Contains(loginErr.Error(), "TESS-42") {
				t.Errorf("error %q is not annotated with the account state", loginErr)
			}
			underlying := tc.requestErr
			if tc.approveErr != nil {
				underlying = tc.approveErr
			} else if tc.waitErr != nil {
				underlying = tc.waitErr
			}
			if !errors.Is(loginErr, underlying) {
				t.Errorf("error %v does not wrap the underlying failure", loginErr)
			}
			if strings.Contains(loginErr.Error(), "ck_secret") {
				t.Errorf("error leaked the connect key: %v", loginErr)
			}
			if builder.registerCalls != 0 {
				t.Errorf("Register calls = %d, want 0 after an earlier failure", builder.registerCalls)
			}
			if fake.activateCalls != 0 {
				t.Errorf("activate calls = %d, want 0", fake.activateCalls)
			}
			saved, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if saved.AppID != "" || saved.AppKey != "" {
				t.Fatalf("partial app credentials persisted: app_id %q app_key %q", saved.AppID, saved.AppKey)
			}
		})
	}
}

// TestAgentRegisterFailureNamesFetchedAccount: the agent path makes a
// best-effort GET /account before minting, so a post-mint failure names the
// billing account instead of "(unknown)".
func TestAgentRegisterFailureNamesFetchedAccount(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	fake := &fakeLoginAPI{connectKey: "ck_agent", account: map[string]any{"billing_id": "TESS-77"}}
	builder, _ := stubHeadlessRegistration(t, fake)
	registerErr := errors.New("indexer unreachable")
	builder.registerErr = registerErr
	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL, BillingAgentKey: "tsk_agent"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	err := agentRegister(cfg, "")
	if err == nil {
		t.Fatal("agentRegister with a failing Register = nil, want the annotated error")
	}
	for _, want := range []string{"registration incomplete", "TESS-77", "minted but not completed", "contact support"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if !errors.Is(err, registerErr) {
		t.Errorf("error %v does not wrap the underlying failure", err)
	}
	if strings.Contains(err.Error(), "ck_agent") {
		t.Errorf("error leaked the connect key: %v", err)
	}
	saved, lerr := loadConfig()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if saved.AppID != "" || saved.AppKey != "" {
		t.Fatalf("partial app credentials persisted: app_id %q app_key %q", saved.AppID, saved.AppKey)
	}
}

// TestAgentRegisterAccountLookupFallback: the account lookup is best-effort —
// a failure renders as "(unknown)" in the failure note and never blocks
// registration.
func TestAgentRegisterAccountLookupFallback(t *testing.T) {
	t.Run("failure note says unknown", func(t *testing.T) {
		testConfigHome(t)
		t.Setenv("TESSERA_PHRASE", "")
		fake := &fakeLoginAPI{connectKey: "ck_agent", accountErr: errors.New("account endpoint down")}
		builder, _ := stubHeadlessRegistration(t, fake)
		builder.registerErr = errors.New("indexer unreachable")
		cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL, BillingAgentKey: "tsk_agent"}
		if err := saveConfig(cfg); err != nil {
			t.Fatal(err)
		}

		err := agentRegister(cfg, "")
		if err == nil {
			t.Fatal("agentRegister = nil, want the annotated failure")
		}
		for _, want := range []string{"registration incomplete", "(unknown)", "minted but not completed", "contact support"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		if builder.registerCalls != 1 {
			t.Errorf("Register calls = %d, want 1: a failed lookup must not block registration", builder.registerCalls)
		}
	})

	t.Run("lookup failure does not block success", func(t *testing.T) {
		testConfigHome(t)
		t.Setenv("TESSERA_PHRASE", "")
		fake := &fakeLoginAPI{connectKey: "ck_agent", accountErr: errors.New("account endpoint down")}
		builder, _ := stubHeadlessRegistration(t, fake)
		cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL, BillingAgentKey: "tsk_agent"}
		if err := saveConfig(cfg); err != nil {
			t.Fatal(err)
		}

		if err := agentRegister(cfg, ""); err != nil {
			t.Fatalf("agentRegister with a failed account lookup: %v", err)
		}
		if builder.requestCalls != 1 || builder.registerCalls != 1 {
			t.Errorf("builder request %d, register %d; want 1/1", builder.requestCalls, builder.registerCalls)
		}
		if fake.activateCalls != 1 {
			t.Errorf("activate calls = %d, want 1", fake.activateCalls)
		}
		saved, lerr := loadConfig()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if saved.AppID == "" || saved.AppKey == "" || saved.BillingAgentKey != "tsk_agent" {
			t.Fatalf("registration did not complete: app_id %q app_key %q agent %q", saved.AppID, saved.AppKey, saved.BillingAgentKey)
		}
	})
}

// TestBillingLoginRegistrationNoPromptNonTerminal is the mutation check for the
// stdin gate: without a terminal, the optional master-password prompt must not
// run (so a scripted login cannot hang), and no phrase is encrypted.
func TestBillingLoginRegistrationNoPromptNonTerminal(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{}}
	stubStdinTerminal(t, false)
	stubBillingClient(t, fake)
	builder := &fakeRegistrationBuilder{
		respURL: "https://index.example/auth/connect/req-1",
		app:     &fakeRegisteredSDK{key: types.GeneratePrivateKey()},
	}
	stubSDKBuilder(t, builder)
	rec := &approveRecorder{}
	stubApproveConnectURL(t, rec, nil)

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}
	for _, banned := range []string{"Protect recovery phrase", "Master password"} {
		if strings.Contains(out, banned) {
			t.Errorf("non-terminal registration showed %q:\n%s", banned, out)
		}
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.PhraseEncrypted != "" || saved.PhraseSalt != "" {
		t.Error("phrase was encrypted on a non-terminal login")
	}
}

// TestBillingLoginRegistrationPromptsOnTerminal: with a terminal the legacy
// optional encryption prompt still runs (here answered "n"), proving the gate
// only suppresses it non-interactively.
func TestBillingLoginRegistrationPromptsOnTerminal(t *testing.T) {
	testConfigHome(t)
	fake := &fakeLoginAPI{session: "sess-1", account: map[string]any{}}
	stubBillingClient(t, fake)
	builder := &fakeRegistrationBuilder{
		respURL: "https://index.example/auth/connect/req-1",
		app:     &fakeRegisteredSDK{key: types.GeneratePrivateKey()},
	}
	stubSDKBuilder(t, builder)
	rec := &approveRecorder{}
	stubApproveConnectURL(t, rec, nil)
	stubStdinTerminal(t, true)
	stubStdin(t, "n\n")

	var loginErr error
	out := captureStdout(t, func() { loginErr = billingLogin("u@example.com", "tok", "") })
	if loginErr != nil {
		t.Fatalf("billingLogin: %v", loginErr)
	}
	if !strings.Contains(out, "Protect recovery phrase with a master password? [Y/n]") {
		t.Errorf("interactive prompt missing from output:\n%s", out)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.PhraseEncrypted != "" || saved.PhraseSalt != "" {
		t.Error(`answering "n" must not save an encrypted phrase`)
	}
}

// TestAgentStorageRegistration pins the plain-login dispatch predicate: only a
// billing agent key with no session and no usable app credentials opts into
// the headless agent path; everything else keeps the legacy browser flow.
func TestAgentStorageRegistration(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"no billing config", &Config{}, false},
		{"session only", &Config{BillingSession: "s"}, false},
		{"agent key only", &Config{BillingAgentKey: "tsk_x"}, true},
		{"agent key with app credentials", &Config{BillingAgentKey: "tsk_x", AppID: "a", AppKey: "k"}, false},
		{"agent key with a partial app id", &Config{BillingAgentKey: "tsk_x", AppID: "a"}, true},
		{"agent key with a partial app key", &Config{BillingAgentKey: "tsk_x", AppKey: "k"}, true},
		{"session and agent key", &Config{BillingSession: "s", BillingAgentKey: "tsk_x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentStorageRegistration(tc.cfg); got != tc.want {
				t.Fatalf("agentStorageRegistration(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

// TestCmdLoginAgentKeyRegistersHeadless is the agent path (spec §6.3.4): with
// only billing_agent_key stored, plain `tessera login` registers headlessly
// instead of starting the legacy browser connection, and the key survives.
func TestCmdLoginAgentKeyRegistersHeadless(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	fake := &fakeLoginAPI{connectKey: "ck_agent"}
	builder, rec := stubHeadlessRegistration(t, fake)
	key := builder.app.(*fakeRegisteredSDK).key
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_agent",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdLogin(nil) })

	if fake.credentialsCalls != 1 || fake.activateCalls != 1 {
		t.Fatalf("credentials %d, activate %d; want 1/1", fake.credentialsCalls, fake.activateCalls)
	}
	if builder.requestCalls != 1 || builder.registerCalls != 1 {
		t.Fatalf("builder request %d, register %d; want 1/1", builder.requestCalls, builder.registerCalls)
	}
	if len(rec.calls) != 1 || rec.calls[0].connectKey != "ck_agent" {
		t.Fatalf("approvals = %+v, want one with the agent-minted connect key", rec.calls)
	}
	if strings.Contains(out, "A browser tab") || strings.Contains(out, "Tessera CLI — Login") {
		t.Errorf("plain login with an agent key took the legacy browser flow:\n%s", out)
	}
	for _, secret := range []string{"ck_agent", "tsk_agent"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaked %q:\n%s", secret, out)
		}
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppID == "" || saved.AppID != builder.appID.String() {
		t.Fatalf("saved AppID = %q, builder got %q", saved.AppID, builder.appID.String())
	}
	if want := hex.EncodeToString([]byte(key)); saved.AppKey != want {
		t.Fatalf("saved AppKey = %q, want %q", saved.AppKey, want)
	}
	if saved.BillingAgentKey != "tsk_agent" {
		t.Fatalf("agent key = %q, want it untouched", saved.BillingAgentKey)
	}
	if saved.BillingSession != "" {
		t.Fatalf("session = %q, want none", saved.BillingSession)
	}
}

// TestLegacyLoginPreservesBillingFields: the legacy completion path rebuilds
// the config around a new storage identity; the stored billing fields — a
// custom billing_api_url included — must survive, both on a first login and on
// a re-login past the already-logged-in guard prompt, which must say so.
func TestLegacyLoginPreservesBillingFields(t *testing.T) {
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

	run := func(t *testing.T, existingAppKey bool) string {
		t.Helper()
		testConfigHome(t)
		t.Setenv("TESSERA_PHRASE", "")
		appKey := ""
		if existingAppKey {
			appKey = strings.Repeat("cd", 32)
		}
		if err := saveConfig(&Config{
			IndexerURL:      defaultIndexer,
			BillingAPIURL:   "https://billing.example.com/api/v1",
			BillingSession:  "sess-live",
			BillingAgentKey: "tsk_keep",
			AppID:           "app-old",
			AppKey:          appKey,
		}); err != nil {
			t.Fatal(err)
		}

		builder := &fakeRegistrationBuilder{
			respURL: "https://index.example/auth/connect/req-1",
			app:     &fakeRegisteredSDK{key: types.GeneratePrivateKey()},
		}
		stubSDKBuilder(t, builder)
		origBrowser := openBrowser
		openBrowser = func(string) error { return nil }
		t.Cleanup(func() { openBrowser = origBrowser })
		if existingAppKey {
			stubStdin(t, "y\n")
		}

		out := captureStdout(t, func() { cmdLogin([]string{phrase}) })

		if builder.requestCalls != 1 || builder.waitCalls != 1 || builder.registerCalls != 1 {
			t.Fatalf("legacy builder calls = request %d, wait %d, register %d; want 1/1/1",
				builder.requestCalls, builder.waitCalls, builder.registerCalls)
		}
		if builder.phrase != phrase {
			t.Fatalf("register phrase = %q, want the positional phrase", builder.phrase)
		}
		saved, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if saved.BillingAPIURL != "https://billing.example.com/api/v1" ||
			saved.BillingSession != "sess-live" || saved.BillingAgentKey != "tsk_keep" {
			t.Fatalf("billing config lost across the legacy login reset: %+v", saved)
		}
		if saved.AppID == "" || saved.AppKey == "" {
			t.Fatalf("storage identity not persisted: app_id %q app_key %q", saved.AppID, saved.AppKey)
		}
		return out
	}

	t.Run("first login", func(t *testing.T) {
		out := run(t, false)
		if !strings.Contains(out, "Logged in.") {
			t.Errorf("completion notice missing:\n%s", out)
		}
	})

	t.Run("re-login keeps billing and says so", func(t *testing.T) {
		out := run(t, true)
		if !strings.Contains(out, "Already logged in") {
			t.Errorf("already-logged-in guard prompt missing:\n%s", out)
		}
		if !strings.Contains(out, "keeps your billing credentials") {
			t.Errorf("guard prompt does not note the preserved billing fields:\n%s", out)
		}
	})
}

// TestPhrasePrintHelpers pins the extracted legacy banner text: the legacy
// login flow and the headless registration share these helpers, so their bytes
// must not drift.
func TestPhrasePrintHelpers(t *testing.T) {
	bar := strings.Repeat("═", 54)
	out := captureStdout(t, func() { printRecoveryPhrase("one two three") })
	want := bar + "\n" +
		"  RECOVERY PHRASE — SAVE THESE 12 WORDS\n" +
		"  They are the ONLY way to recover your account.\n" +
		"  We do not store them.\n" +
		bar + "\n" +
		"  one two three\n" +
		bar + "\n\n"
	if out != want {
		t.Errorf("printRecoveryPhrase output:\n%q\nwant:\n%q", out, want)
	}

	out = captureStdout(t, func() { printSuppliedPhraseNote() })
	want = "Using the recovery phrase you supplied — the app key is derived\n" +
		"from it, so this account is reproducible on any machine.\n\n"
	if out != want {
		t.Errorf("printSuppliedPhraseNote output:\n%q\nwant:\n%q", out, want)
	}
}
