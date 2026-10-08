package main

import (
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
)

// captureStdout runs f with os.Stdout redirected to a pipe and returns what f
// printed. The command helpers resolve os.Stdout at call time, so the swap is
// observed even though they also use fmt.Print.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// testConfigHome points TESSERA_HOME at a fresh directory so config tests never
// touch the real ~/.tessera.
func testConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("TESSERA_HOME", home)
	return home
}

// assertSignInGuidance fails unless msg is the shared billing 401 guidance:
// signup/login pointers plus the revoked-agent-key recovery, with the API
// error it explains still attached.
func assertSignInGuidance(t *testing.T, source, msg string) {
	t.Helper()
	for _, want := range []string{"tessera signup", "tessera login", "signup --force", "HTTP 401"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s guidance %q missing %q", source, msg, want)
		}
	}
}

func TestParseBillingError(t *testing.T) {
	t.Run("nested detail.error preserves siblings", func(t *testing.T) {
		be := parseBillingError(409, []byte(`{"detail":{"error":"below_asset_minimum","minimum_usd":"10.00"}}`))
		if be.Status != 409 {
			t.Fatalf("Status = %d, want 409", be.Status)
		}
		if be.Code != "below_asset_minimum" {
			t.Fatalf("Code = %q, want below_asset_minimum", be.Code)
		}
		if got := be.Extra["minimum_usd"]; got != "10.00" {
			t.Fatalf("Extra[minimum_usd] = %v, want 10.00", got)
		}
		if !strings.Contains(be.Error(), "409") || !strings.Contains(be.Error(), "below_asset_minimum") {
			t.Fatalf("Error() = %q, want the status and code", be.Error())
		}
	})

	t.Run("flat error preserves siblings", func(t *testing.T) {
		be := parseBillingError(502, []byte(`{"error":"settlement_pending","transaction":"0xdead"}`))
		if be.Status != 502 || be.Code != "settlement_pending" {
			t.Fatalf("got status %d code %q", be.Status, be.Code)
		}
		if got := be.Extra["transaction"]; got != "0xdead" {
			t.Fatalf("Extra[transaction] = %v, want 0xdead", got)
		}
	})

	t.Run("flat 401 invalid_token", func(t *testing.T) {
		be := parseBillingError(401, []byte(`{"error":"invalid_token"}`))
		if be.Status != 401 || be.Code != "invalid_token" {
			t.Fatalf("got status %d code %q", be.Status, be.Code)
		}
	})

	t.Run("x402 challenge keeps accepts and challenge_id", func(t *testing.T) {
		body := `{"x402Version":2,"error":"invalid_payload","accepts":[{"scheme":"exact","network":"base","amount":"5000000","payTo":"0xabc"}],"challenge_id":7,"payment_intent_id":3}`
		be := parseBillingError(402, []byte(body))
		if be.Status != 402 || be.Code != "invalid_payload" {
			t.Fatalf("got status %d code %q", be.Status, be.Code)
		}
		accepts, ok := be.Extra["accepts"].([]any)
		if !ok || len(accepts) != 1 {
			t.Fatalf("Extra[accepts] = %#v, want one entry", be.Extra["accepts"])
		}
		first, ok := accepts[0].(map[string]any)
		if !ok || first["network"] != "base" {
			t.Fatalf("accepts[0] = %#v", accepts[0])
		}
		if got := be.Extra["challenge_id"]; got != float64(7) {
			t.Fatalf("Extra[challenge_id] = %v, want 7", got)
		}
	})

	t.Run("non-json body is kept as the code", func(t *testing.T) {
		be := parseBillingError(503, []byte("service unavailable"))
		if be.Status != 503 || be.Code != "service unavailable" {
			t.Fatalf("got status %d code %q", be.Status, be.Code)
		}
	})
}

func TestIsBillingURL(t *testing.T) {
	valid := []string{
		defaultBillingURL,
		"https://billing.example.com",
		"https://billing.example.com/api/v1",
		"https://billing.example.com:8443/x",
		"http://127.0.0.1/api/v1",
		"http://127.0.0.1:8080/api/v1",
		"http://localhost:9000",
		"http://LOCALHOST/api/v1",
	}
	for _, raw := range valid {
		if !isBillingURL(raw) {
			t.Errorf("isBillingURL(%q) = false, want true", raw)
		}
	}
	invalid := []string{
		"",
		"tessera.storage/api/v1",
		"ftp://tessera.storage/api/v1",
		"http://tessera.storage/api/v1",
		"http://127.0.0.1.evil.com/api/v1",
		"http://localhost.evil.com/api/v1",
		"https://",
		"://missing-scheme",
	}
	for _, raw := range invalid {
		if isBillingURL(raw) {
			t.Errorf("isBillingURL(%q) = true, want false", raw)
		}
	}
}

func TestApplyBillingURL(t *testing.T) {
	cfg := &Config{BillingAPIURL: "https://keep.example"}
	if err := applyBillingURL(cfg, "http://evil.example.com/api/v1"); err == nil {
		t.Fatal("applyBillingURL accepted http on a public host")
	}
	if cfg.BillingAPIURL != "https://keep.example" {
		t.Fatalf("an invalid URL must not change the config, got %q", cfg.BillingAPIURL)
	}
	if err := applyBillingURL(cfg, "https://billing.example.com/api/v1/"); err != nil {
		t.Fatalf("applyBillingURL(valid): %v", err)
	}
	if cfg.BillingAPIURL != "https://billing.example.com/api/v1" {
		t.Fatalf("BillingAPIURL = %q, want the trailing slash trimmed", cfg.BillingAPIURL)
	}
}

func TestConfigBillingRoundTrip(t *testing.T) {
	home := testConfigHome(t)
	cp := filepath.Join(home, configFileName)

	// A config written before the billing fields existed must keep loading,
	// with the billing URL defaulted and no credentials invented.
	legacy := `{"indexer_url":"` + defaultIndexer + `","app_id":"abcd"}`
	if err := os.WriteFile(cp, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig(legacy): %v", err)
	}
	if cfg.BillingAPIURL != defaultBillingURL {
		t.Errorf("BillingAPIURL = %q, want the default %q", cfg.BillingAPIURL, defaultBillingURL)
	}
	if cfg.BillingSession != "" || cfg.BillingAgentKey != "" {
		t.Errorf("legacy config must not gain credentials: %+v", cfg)
	}

	cfg.BillingAPIURL = "http://127.0.0.1:9999/api/v1"
	cfg.BillingSession = "session-secret"
	cfg.BillingAgentKey = "tsk_agent_secret"
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	fi, err := os.Stat(cp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %o, want 0600", fi.Mode().Perm())
	}

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.BillingAPIURL != cfg.BillingAPIURL || got.BillingSession != cfg.BillingSession || got.BillingAgentKey != cfg.BillingAgentKey {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, cfg)
	}
}

func TestConfigJSONOmitsSecrets(t *testing.T) {
	testConfigHome(t)
	cfg := &Config{
		IndexerURL:      defaultIndexer,
		AppID:           "abcd",
		BillingAPIURL:   "https://billing.example.com/api/v1",
		BillingSession:  "session-secret",
		BillingAgentKey: "tsk_agent_secret",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdConfig([]string{"--json"}) })
	if strings.Contains(out, "session-secret") || strings.Contains(out, "tsk_agent_secret") {
		t.Fatalf("config --json leaked a credential:\n%s", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("config --json is not JSON: %v\n%s", err, out)
	}
	if m["billing_api_url"] != "https://billing.example.com/api/v1" {
		t.Errorf("billing_api_url = %v, want the configured URL", m["billing_api_url"])
	}
	if m["billing_session_set"] != true || m["billing_agent_key_set"] != true {
		t.Errorf("credential presence flags missing: session=%v agent=%v",
			m["billing_session_set"], m["billing_agent_key_set"])
	}
}

func TestCmdConfigBillingURL(t *testing.T) {
	testConfigHome(t)
	if err := saveConfig(&Config{IndexerURL: defaultIndexer, AppID: "abcd"}); err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		cmdConfig([]string{"--billing-url", "https://billing.example.com/api/v1/"})
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BillingAPIURL != "https://billing.example.com/api/v1" {
		t.Fatalf("BillingAPIURL = %q, want the validated URL with the trailing slash trimmed", cfg.BillingAPIURL)
	}
}

func TestNewBillingAPI(t *testing.T) {
	if api := newBillingAPI(&Config{}); api.base != defaultBillingURL {
		t.Fatalf("base = %q, want %q", api.base, defaultBillingURL)
	}
	if api := newBillingAPI(&Config{BillingAPIURL: "https://billing.example.com/api/v1/"}); api.base != "https://billing.example.com/api/v1" {
		t.Fatalf("base = %q, want the trailing slash trimmed", api.base)
	}
	cfg := &Config{BillingSession: "s", BillingAgentKey: "k"}
	api := newBillingAPI(cfg)
	if api.session != "s" || api.agentKey != "k" {
		t.Fatalf("credentials not carried over: %+v", api)
	}

	// The env override wins over the configured URL (dev/test escape hatch).
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		fmt.Fprint(w, `{"account_id":"AGNT-1","api_key":"tsk_1"}`)
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL+"/")
	overridden := newBillingAPI(&Config{BillingAPIURL: "https://configured.invalid/api/v1"})
	if _, _, err := overridden.CreateAgent("", ""); err != nil {
		t.Fatalf("CreateAgent through the env override: %v", err)
	}
	if hit != "/agents" {
		t.Fatalf("request path = %q, want the override server to receive /agents", hit)
	}
}

func TestHTTPBillingAuthSelection(t *testing.T) {
	var lastPath, lastAuth, lastCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastAuth = r.Header.Get("Authorization")
		lastCookie = ""
		if c, err := r.Cookie(sessionCookieName); err == nil {
			lastCookie = c.Value
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/agents":
			fmt.Fprint(w, `{"account_id":"AGNT-1","api_key":"tsk_1"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	t.Run("session wins when both credentials are set", func(t *testing.T) {
		api := &httpBilling{base: srv.URL, client: srv.Client(), session: "cookie-value", agentKey: "tsk_key"}
		if _, err := api.Account(); err != nil {
			t.Fatal(err)
		}
		if lastPath != "/account" {
			t.Fatalf("path = %q, want /account", lastPath)
		}
		if lastAuth != "" {
			t.Fatalf("Authorization = %q, want none: a stored session must win over the agent key", lastAuth)
		}
		if lastCookie != "cookie-value" {
			t.Fatalf("cookie = %q, want the session cookie", lastCookie)
		}
	})

	t.Run("agent key when only a bearer is set", func(t *testing.T) {
		api := &httpBilling{base: srv.URL, client: srv.Client(), agentKey: "tsk_key"}
		if _, err := api.Account(); err != nil {
			t.Fatal(err)
		}
		if lastAuth != "Bearer tsk_key" {
			t.Fatalf("Authorization = %q, want the bearer key", lastAuth)
		}
		if lastCookie != "" {
			t.Fatalf("cookie = %q, want none when bearer auth is selected", lastCookie)
		}
	})

	t.Run("cookie when only a session is set", func(t *testing.T) {
		api := &httpBilling{base: srv.URL, client: srv.Client(), session: "cookie-value"}
		if _, err := api.Account(); err != nil {
			t.Fatal(err)
		}
		if lastAuth != "" || lastCookie != "cookie-value" {
			t.Fatalf("auth = %q cookie = %q, want the session cookie only", lastAuth, lastCookie)
		}
	})

	t.Run("no credentials is a clear error", func(t *testing.T) {
		api := &httpBilling{base: srv.URL, client: srv.Client()}
		if _, err := api.Account(); !errors.Is(err, errNoBillingCredentials) {
			t.Fatalf("Account() error = %v, want errNoBillingCredentials", err)
		}
	})

	t.Run("public endpoints send no credentials", func(t *testing.T) {
		api := &httpBilling{base: srv.URL, client: srv.Client(), session: "cookie-value", agentKey: "tsk_key"}
		if err := api.MagicLink("user@example.com"); err != nil {
			t.Fatal(err)
		}
		if lastPath != "/auth/magic-link" || lastAuth != "" || lastCookie != "" {
			t.Fatalf("magic-link sent path=%q auth=%q cookie=%q", lastPath, lastAuth, lastCookie)
		}
		if _, _, err := api.CreateAgent("", ""); err != nil {
			t.Fatal(err)
		}
		if lastPath != "/agents" || lastAuth != "" || lastCookie != "" {
			t.Fatalf("agents signup sent path=%q auth=%q cookie=%q", lastPath, lastAuth, lastCookie)
		}
	})

	t.Run("logout uses the cookie, never the bearer", func(t *testing.T) {
		api := &httpBilling{base: srv.URL, client: srv.Client(), session: "old-session", agentKey: "tsk_key"}
		if err := api.Logout("new-session"); err != nil {
			t.Fatal(err)
		}
		if lastPath != "/auth/logout" || lastAuth != "" || lastCookie != "new-session" {
			t.Fatalf("logout sent path=%q auth=%q cookie=%q", lastPath, lastAuth, lastCookie)
		}
	})
}

func TestHTTPBillingVerifyCapturesSession(t *testing.T) {
	var lastAuth, lastCookie, lastToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		lastCookie = ""
		if c, err := r.Cookie(sessionCookieName); err == nil {
			lastCookie = c.Value
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastToken, _ = body["token"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "tessera_session=verified-session; Path=/; HttpOnly; Secure; SameSite=Lax")
		fmt.Fprint(w, `{"billing_id":"TESS-1"}`)
	}))
	defer srv.Close()

	// Even with credentials configured, verify is a public endpoint: the
	// stale cookie must not be replayed and no bearer may be attached.
	api := &httpBilling{base: srv.URL, client: srv.Client(), session: "stale", agentKey: "tsk_old"}
	session, err := api.Verify("magic-token")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if session != "verified-session" {
		t.Fatalf("session = %q, want the Set-Cookie value", session)
	}
	if lastToken != "magic-token" {
		t.Fatalf("token body = %q, want the supplied token", lastToken)
	}
	if lastAuth != "" || lastCookie != "" {
		t.Fatalf("verify sent auth: %q / %q", lastAuth, lastCookie)
	}

	t.Run("missing Set-Cookie is an error", func(t *testing.T) {
		bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{}`)
		}))
		defer bare.Close()
		api := &httpBilling{base: bare.URL, client: bare.Client()}
		if _, err := api.Verify("t"); err == nil || !strings.Contains(err.Error(), sessionCookieName) {
			t.Fatalf("Verify error = %v, want a missing-%s error", err, sessionCookieName)
		}
	})
}

func TestHTTPBillingErrorShapes(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		call     func(*httpBilling) error
		wantCode string
		extraKey string
		extraVal any
	}{
		{
			name:   "account 401 via agent key",
			status: http.StatusUnauthorized,
			body:   `{"detail":{"error":"unauthorized"}}`,
			call: func(api *httpBilling) error {
				_, err := api.Account()
				return err
			},
			wantCode: "unauthorized",
		},
		{
			name:   "credentials 409 identity_exists",
			status: http.StatusConflict,
			body:   `{"detail":{"error":"identity_exists"}}`,
			call: func(api *httpBilling) error {
				_, err := api.Credentials()
				return err
			},
			wantCode: "identity_exists",
		},
		{
			name:   "topup 409 below asset minimum",
			status: http.StatusConflict,
			body:   `{"detail":{"error":"below_asset_minimum","minimum_usd":"10.00"}}`,
			call: func(api *httpBilling) error {
				_, err := api.Topup("5", "nowpayments", "btc", "")
				return err
			},
			wantCode: "below_asset_minimum",
			extraKey: "minimum_usd",
			extraVal: "10.00",
		},
		{
			name:   "topup 422 amount out of range",
			status: http.StatusUnprocessableEntity,
			body:   `{"detail":{"error":"amount_out_of_range"}}`,
			call: func(api *httpBilling) error {
				_, err := api.Topup("1", "", "", "")
				return err
			},
			wantCode: "amount_out_of_range",
		},
		{
			name:   "topup 402 challenge",
			status: http.StatusPaymentRequired,
			body:   `{"x402Version":2,"error":"payment_required","accepts":[{"scheme":"exact","network":"base"}],"challenge_id":7}`,
			call: func(api *httpBilling) error {
				_, err := api.Topup("5", "x402", "", "")
				return err
			},
			wantCode: "payment_required",
			extraKey: "accepts",
		},
		{
			name:   "topup 502 settlement pending",
			status: http.StatusBadGateway,
			body:   `{"error":"settlement_pending","transaction":"0xabc"}`,
			call: func(api *httpBilling) error {
				_, err := api.Topup("5", "", "", "cGF5")
				return err
			},
			wantCode: "settlement_pending",
			extraKey: "transaction",
			extraVal: "0xabc",
		},
		{
			name:   "topup 503 payments unconfigured",
			status: http.StatusServiceUnavailable,
			body:   `{"detail":{"error":"payments_unconfigured"}}`,
			call: func(api *httpBilling) error {
				_, err := api.Topup("5", "nowpayments", "", "")
				return err
			},
			wantCode: "payments_unconfigured",
		},
		{
			name:   "promo 404",
			status: http.StatusNotFound,
			body:   `{"detail":{"error":"promo_not_found"}}`,
			call: func(api *httpBilling) error {
				_, err := api.Promo("NOPE")
				return err
			},
			wantCode: "promo_not_found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			// An agent key keeps these cases in the bearer path, where a 401
			// is an ordinary API error rather than session expiry.
			api := &httpBilling{base: srv.URL, client: srv.Client(), agentKey: "tsk_key"}
			err := tc.call(api)
			if err == nil {
				t.Fatal("want an error")
			}
			var be *billingError
			if !errors.As(err, &be) {
				t.Fatalf("error = %v (%T), want *billingError", err, err)
			}
			if be.Status != tc.status || be.Code != tc.wantCode {
				t.Fatalf("billingError = {Status:%d Code:%q}, want {%d %q}", be.Status, be.Code, tc.status, tc.wantCode)
			}
			if tc.extraKey != "" {
				got, ok := be.Extra[tc.extraKey]
				if !ok {
					t.Fatalf("Extra[%s] missing: %#v", tc.extraKey, be.Extra)
				}
				if tc.extraVal != nil && got != tc.extraVal {
					t.Fatalf("Extra[%s] = %v, want %v", tc.extraKey, got, tc.extraVal)
				}
			}
		})
	}
}

func TestHTTPBillingTopupRequestShape(t *testing.T) {
	var lastBody map[string]any
	var lastPath, lastXPayment, lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		lastXPayment = r.Header.Get("X-PAYMENT")
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"payment_intent_id":1,"invoice_url":"https://pay.example/i","expires_at":"2026-10-08T13:00:00Z"}`)
	}))
	defer srv.Close()
	api := &httpBilling{base: srv.URL, client: srv.Client(), agentKey: "tsk_key"}

	resp, err := api.Topup("12.50", "x402", "usdc", "cHJlLXNpZ25lZA==")
	if err != nil {
		t.Fatalf("Topup: %v", err)
	}
	if lastPath != "/billing/topup" {
		t.Fatalf("path = %q, want /billing/topup", lastPath)
	}
	if resp["invoice_url"] != "https://pay.example/i" {
		t.Fatalf("resp = %#v, want the parsed invoice body", resp)
	}
	if lastBody["amount_usd"] != "12.50" || lastBody["rail"] != "x402" || lastBody["pay_currency"] != "usdc" {
		t.Fatalf("body = %#v", lastBody)
	}
	if lastXPayment != "cHJlLXNpZ25lZA==" {
		t.Fatalf("X-PAYMENT = %q, want the header passed through unchanged", lastXPayment)
	}
	if lastAuth != "Bearer tsk_key" {
		t.Fatalf("Authorization = %q", lastAuth)
	}

	// Amount-only topup omits the optional fields entirely.
	if _, err := api.Topup("5", "", "", ""); err != nil {
		t.Fatalf("Topup amount only: %v", err)
	}
	if len(lastBody) != 1 || lastBody["amount_usd"] != "5" {
		t.Fatalf("body = %#v, want only amount_usd", lastBody)
	}
	if lastXPayment != "" {
		t.Fatalf("X-PAYMENT = %q, want none", lastXPayment)
	}
}

func TestHTTPBillingHappyShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/account":
			fmt.Fprint(w, `{"billing_id":"TESS-1","balance_usd":"12.5","period_end":null,"days_left":null}`)
		case "/credentials":
			fmt.Fprint(w, `{"connect_key":"ck_1","quota":"plan-payg"}`)
		case "/credentials/activate":
			fmt.Fprint(w, `{"linked":true}`)
		case "/billing/promo":
			fmt.Fprint(w, `{"promo_id":1,"kind":"credit","value":"5","periods_remaining":1}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	api := &httpBilling{base: srv.URL, client: srv.Client(), agentKey: "tsk_key"}

	acct, err := api.Account()
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if acct["balance_usd"] != "12.5" || acct["period_end"] != nil {
		t.Fatalf("account = %#v, want parsed values with nulls preserved", acct)
	}
	key, err := api.Credentials()
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if key != "ck_1" {
		t.Fatalf("connect key = %q, want ck_1", key)
	}
	if err := api.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	promo, err := api.Promo("CODE")
	if err != nil {
		t.Fatalf("Promo: %v", err)
	}
	if promo["kind"] != "credit" {
		t.Fatalf("promo = %#v", promo)
	}
}

func TestSessionExpiryClearsConfig(t *testing.T) {
	testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":{"error":"unauthorized"}}`)
	}))
	defer srv.Close()

	cfg := &Config{IndexerURL: defaultIndexer, AppID: "abcd", BillingAPIURL: srv.URL, BillingSession: "dead-session"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	api := newBillingAPI(cfg)
	_, err := api.Account()
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("Account() error = %v (%T), want errSessionExpired", err, err)
	}
	if !strings.Contains(err.Error(), "tessera login --email") {
		t.Fatalf("expiry guidance = %q, want the login guidance", err.Error())
	}
	reloaded, lerr := loadConfig()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if reloaded.BillingSession != "" {
		t.Fatalf("BillingSession = %q, want it cleared after a cookie 401", reloaded.BillingSession)
	}
	if reloaded.BillingAgentKey != cfg.BillingAgentKey {
		t.Fatalf("agent key changed: %q", reloaded.BillingAgentKey)
	}
}

func TestAgentKeyUnaffectedBy401(t *testing.T) {
	testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":{"error":"unauthorized"}}`)
	}))
	defer srv.Close()

	// Agent-only: with no session stored, a 401 is an ordinary bearer error,
	// never the session-expiry signal.
	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: srv.URL, BillingAgentKey: "tsk_key"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	api := newBillingAPI(cfg)
	_, err := api.Account()
	if errors.Is(err, errSessionExpired) {
		t.Fatal("a bearer 401 must not be reported as session expiry")
	}
	var be *billingError
	if !errors.As(err, &be) || be.Status != http.StatusUnauthorized {
		t.Fatalf("error = %v, want a billingError with status 401", err)
	}
	reloaded, lerr := loadConfig()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if reloaded.BillingAgentKey != "tsk_key" {
		t.Fatalf("a bearer 401 dropped the agent key: %q", reloaded.BillingAgentKey)
	}
	if reloaded.BillingSession != "" {
		t.Fatalf("BillingSession = %q, want none", reloaded.BillingSession)
	}
}

// TestSession401ClearsAndFallsBackToAgentKey is the fallback half of the
// precedence ruling: the stored session is used first; when the server rejects
// it the dead cookie is dropped in memory and in the config, so the next
// authAuto call (same client or a fresh one built from the config) uses the
// stored agent key instead of replaying the cookie.
func TestSession401ClearsAndFallsBackToAgentKey(t *testing.T) {
	testConfigHome(t)
	var auths, cookies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		cookie := ""
		if c, err := r.Cookie(sessionCookieName); err == nil {
			cookie = c.Value
		}
		cookies = append(cookies, cookie)
		w.Header().Set("Content-Type", "application/json")
		if len(auths) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"detail":{"error":"unauthorized"}}`)
			return
		}
		fmt.Fprint(w, `{"billing_id":"TESS-9"}`)
	}))
	defer srv.Close()

	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: srv.URL, BillingSession: "sess-dead", BillingAgentKey: "tsk_live"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	api := newBillingAPI(cfg)
	if _, err := api.Account(); !errors.Is(err, errSessionExpired) {
		t.Fatalf("first Account() error = %v (%T), want errSessionExpired", err, err)
	}
	if cookies[0] != "sess-dead" || auths[0] != "" {
		t.Fatalf("first call sent cookie=%q auth=%q, want the stored session", cookies[0], auths[0])
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.BillingSession != "" {
		t.Fatalf("dead session still stored: %q", saved.BillingSession)
	}

	// Same client: the rejected session was dropped in memory too.
	acct, err := api.Account()
	if err != nil {
		t.Fatalf("second Account() call: %v", err)
	}
	if acct["billing_id"] != "TESS-9" {
		t.Fatalf("second Account() = %#v", acct)
	}
	if len(auths) != 2 || auths[1] != "Bearer tsk_live" || cookies[1] != "" {
		t.Fatalf("second call sent cookie=%q auth=%q, want the agent key", cookies[1], auths[1])
	}

	// A fresh client built from the reloaded config behaves the same.
	fresh := newBillingAPI(saved)
	if _, err := fresh.Account(); err != nil {
		t.Fatalf("fresh client Account(): %v", err)
	}
	if len(auths) != 3 || auths[2] != "Bearer tsk_live" || cookies[2] != "" {
		t.Fatalf("fresh client sent cookie=%q auth=%q, want the agent key", cookies[2], auths[2])
	}
}
