package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// accountServerJSON mirrors moneyd's GET /account projection
// (dataplane/moneyd/routers/account.py): all 15 fields, with period_end and
// days_left null when the account has nothing to burn (dunning.py:171-175).
// network_bytes is indexd's redundant network total; raw_bytes is the sum of
// live object slab lengths. balance_usd is the JSON number FastAPI sends, not
// a quoted decimal.
const accountServerJSON = `{
  "billing_id": "TESS-000000000042",
  "kind": "human",
  "status": "active",
  "balance_usd": 7.5,
  "plan": "payg",
  "period_end": null,
  "included_storage_bytes": 0,
  "network_bytes": 377487360,
  "raw_bytes": 125829120,
  "open_slabs": 3,
  "allowance_egress_bytes": 5000000,
  "egress_used_bytes": 1250000,
  "days_left": 29,
  "topup_url": "https://tessera.storage/account",
  "warnings": []
}`

func accountObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode test account JSON: %v", err)
	}
	return m
}

// fakeAccountAPI wires only Account; the embedded nil interface makes any
// other call panic, which is part of the assertion that `account` speaks only
// to GET /account.
type fakeAccountAPI struct {
	billingAPI
	calls int
	acct  map[string]any
	err   error
	cfg   *Config
}

func (f *fakeAccountAPI) Account() (map[string]any, error) {
	f.calls++
	return f.acct, f.err
}

// stubAccountClient swaps the client-construction seam and records the config
// the command resolved.
func stubAccountClient(t *testing.T, fake *fakeAccountAPI) {
	t.Helper()
	orig := newBillingClient
	newBillingClient = func(cfg *Config) billingAPI {
		fake.cfg = cfg
		return fake
	}
	t.Cleanup(func() { newBillingClient = orig })
}

// lineWithPrefix returns the trimmed output line starting with prefix ("" when
// there is none), so table assertions can pin a whole row.
func lineWithPrefix(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return line
		}
	}
	return ""
}

// TestCredentialLabel pins the tag `account` reports for the stored
// credential: a saved human session wins over an agent key (the precedence
// ruling), and "" means neither is stored.
func TestCredentialLabel(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"none", &Config{}, ""},
		{"agent key only", &Config{BillingAgentKey: "tsk_x"}, "agent"},
		{"session only", &Config{BillingSession: "s"}, "session"},
		{"session wins over agent key", &Config{BillingSession: "s", BillingAgentKey: "tsk_x"}, "session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialLabel(tc.cfg); got != tc.want {
				t.Fatalf("credentialLabel(%+v) = %q, want %q", tc.cfg, got, tc.want)
			}
		})
	}
}

// TestPrintAccountTableNulls: period_end and days_left are null for zero-burn
// accounts and must render as an em dash, never as a Go formatting artifact.
func TestPrintAccountTableNulls(t *testing.T) {
	acct := map[string]any{
		"billing_id":             "TESS-1",
		"kind":                   "agent",
		"status":                 "pending",
		"balance_usd":            "0",
		"plan":                   "payg",
		"period_end":             nil,
		"allowance_egress_bytes": float64(0),
		"egress_used_bytes":      float64(0),
		"days_left":              nil,
		"topup_url":              "https://tessera.storage/account",
	}
	out := captureStdout(t, func() { printAccountTable(acct, "agent") })

	if strings.Contains(out, "%!") || strings.Contains(out, "<nil>") || strings.Contains(out, "nil") {
		t.Fatalf("null field rendered as a formatting artifact:\n%s", out)
	}
	if line := lineWithPrefix(out, "Period"); !strings.HasSuffix(strings.TrimSpace(line), "—") && !strings.Contains(line, "pay as you go") {
		t.Errorf("period_end null rendered %q, want an em dash or the pay-as-you-go note", line)
	}
	if line := lineWithPrefix(out, "Days left"); !strings.HasSuffix(strings.TrimSpace(line), "—") {
		t.Errorf("days_left null rendered %q, want an em dash or the pay-as-you-go note", line)
	}
	for _, want := range []string{
		"TESS-1",
		"agent",
		"pending",
		"$0",
		"payg",
		"measured host spend", // retired egress byte counter is not shown as 0 B
		"agent key",
		"https://tessera.storage/account",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// TestAccountBalanceFormatting pins the human balance rendering for FastAPI's
// numeric balance_usd: exactly two decimals, and never scientific notation for
// large values. A quoted decimal is a legacy/defensive shape passed through.
func TestAccountBalanceFormatting(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{"numeric fraction", 7.5, "7.50"},
		{"numeric integer", float64(7), "7.00"},
		{"numeric zero", float64(0), "0.00"},
		{"large value avoids scientific notation", 1e21, "1000000000000000000000.00"},
		{"quoted decimal passes through", "7.50", "7.50"},
		{"missing", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := map[string]any{}
			if tc.v != nil {
				acct["balance_usd"] = tc.v
			}
			if got := accountBalanceUSD(acct, "balance_usd"); got != tc.want {
				t.Fatalf("accountBalanceUSD(%#v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}

	// The rendered table must carry the cents form and never "e+".
	acct := map[string]any{"billing_id": "TESS-1", "balance_usd": 1e21}
	out := captureStdout(t, func() { printAccountTable(acct, "agent") })
	if !strings.Contains(out, "$1000000000000000000000.00") {
		t.Errorf("large balance missing the cents form:\n%s", out)
	}
	if strings.Contains(out, "e+") || strings.Contains(out, "E+") {
		t.Errorf("large balance rendered in scientific notation:\n%s", out)
	}
}

// TestCmdAccountHumanTable drives the command end-to-end through the fake:
// identity, balance, plan, byte formatting, days left, the active credential,
// and the top-up URL, with no secret anywhere in the output.
func TestCmdAccountHumanTable(t *testing.T) {
	testConfigHome(t)
	fake := &fakeAccountAPI{acct: accountObject(t, accountServerJSON)}
	stubAccountClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingSession:  "sess-live",
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdAccount(nil) })

	if fake.calls != 1 {
		t.Fatalf("Account calls = %d, want 1", fake.calls)
	}
	if fake.cfg == nil || fake.cfg.BillingSession != "sess-live" || fake.cfg.BillingAgentKey != "tsk_secret" {
		t.Fatalf("client config = %+v, want both stored credentials", fake.cfg)
	}
	for _, want := range []string{
		"TESS-000000000042",
		"human",
		"active",
		"$7.50",
		"payg",
		"4.77 MiB",            // allowance_egress_bytes = 5,000,000
		"measured host spend", // downloads row replaces the retired byte counter
		"29",
		"session", // the stored session wins over the agent key
		"https://tessera.storage/account",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human table missing %q:\n%s", want, out)
		}
	}
	if line := lineWithPrefix(out, "Credential"); !strings.Contains(line, "session") {
		t.Errorf("credential line = %q, want the session", line)
	}
	if line := lineWithPrefix(out, "Period"); !strings.HasSuffix(strings.TrimSpace(line), "—") && !strings.Contains(line, "pay as you go") {
		t.Errorf("null period_end rendered %q, want an em dash or the pay-as-you-go note", line)
	}
	for _, secret := range []string{"sess-live", "tsk_secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("human table leaked a credential %q:\n%s", secret, out)
		}
	}
}

// TestCmdAccountShowsAgentCredential: with no session stored the table names
// the agent key as the active credential.
func TestCmdAccountShowsAgentCredential(t *testing.T) {
	testConfigHome(t)
	fake := &fakeAccountAPI{acct: accountObject(t, accountServerJSON)}
	stubAccountClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdAccount(nil) })
	if line := lineWithPrefix(out, "Credential"); !strings.Contains(line, "agent key") {
		t.Fatalf("credential line = %q, want the agent key", line)
	}
}

// TestCmdAccountJSONVerbatim pins the --json contract: every server field is
// passed through unchanged (balance_usd stays the server's number 7.5; nulls
// stay null) plus exactly one added credential tag, and no secret appears.
func TestCmdAccountJSONVerbatim(t *testing.T) {
	testConfigHome(t)
	server := accountObject(t, accountServerJSON)
	fake := &fakeAccountAPI{acct: server}
	stubAccountClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingSession:  "sess-live",
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdAccount([]string{"--json"}) })
	for _, secret := range []string{"sess-live", "tsk_secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("--json leaked a credential %q:\n%s", secret, out)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	// credential + egress_note are the CLI's additions (no readiness block in
	// this fixture, so no "trial" object).
	if len(got) != len(server)+2 {
		t.Fatalf("--json has %d keys, want the %d server fields plus credential and egress_note", len(got), len(server))
	}
	if got["egress_used_bytes"] != nil || got["egress_note"] == nil {
		t.Errorf("retired egress counter must be null with a note: %v / %v", got["egress_used_bytes"], got["egress_note"])
	}
	for k, v := range server {
		if k == "egress_used_bytes" {
			continue
		}
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("--json[%q] = %#v, want the server value %#v", k, got[k], v)
		}
	}
	if got["credential"] != "session" {
		t.Errorf("credential = %v, want session (a stored session wins over the agent key)", got["credential"])
	}
	if bal, ok := got["balance_usd"].(float64); !ok || bal != 7.5 {
		t.Errorf("balance_usd = %#v, want the server's number 7.5 preserved verbatim", got["balance_usd"])
	}
	if got["period_end"] != nil {
		t.Errorf("period_end = %#v, want the server's null preserved", got["period_end"])
	}
	if d, ok := got["days_left"].(float64); !ok || d != 29 {
		t.Errorf("days_left = %#v, want the server's number preserved", got["days_left"])
	}

	// With only an agent key stored, the tag flips to agent.
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { cmdAccount([]string{"--json"}) })
	var agentOut map[string]any
	if err := json.Unmarshal([]byte(out), &agentOut); err != nil {
		t.Fatalf("agent --json output is not JSON: %v\n%s", err, out)
	}
	if agentOut["credential"] != "agent" {
		t.Errorf("agent credential = %v, want agent", agentOut["credential"])
	}
}

// TestAccountGuidanceWithoutBillingConfig: no config file at all and a config
// with no billing credential both fail fast with the signup/login pointer,
// without calling the API.
func TestAccountGuidanceWithoutBillingConfig(t *testing.T) {
	t.Run("no config file", func(t *testing.T) {
		testConfigHome(t)
		fake := &fakeAccountAPI{acct: accountObject(t, accountServerJSON)}
		stubAccountClient(t, fake)

		_, _, err := fetchAccountSummary()
		if err == nil {
			t.Fatal("fetchAccountSummary with no config = nil, want guidance")
		}
		if !errors.Is(err, errNoBillingCredentials) {
			t.Fatalf("error = %v (%T), want errNoBillingCredentials", err, err)
		}
		for _, want := range []string{"tessera signup", "tessera login"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		if fake.calls != 0 {
			t.Fatalf("Account calls = %d, want 0 without credentials", fake.calls)
		}
	})

	t.Run("config without billing credentials", func(t *testing.T) {
		testConfigHome(t)
		if err := saveConfig(&Config{IndexerURL: defaultIndexer, AppID: "app-1"}); err != nil {
			t.Fatal(err)
		}
		fake := &fakeAccountAPI{acct: accountObject(t, accountServerJSON)}
		stubAccountClient(t, fake)

		_, _, err := fetchAccountSummary()
		if !errors.Is(err, errNoBillingCredentials) {
			t.Fatalf("error = %v (%T), want errNoBillingCredentials", err, err)
		}
		if fake.calls != 0 {
			t.Fatalf("Account calls = %d, want 0 without credentials", fake.calls)
		}
	})
}

// TestAccount401Guidance: a stored agent key the server rejects (the sessionless
// 401) must not surface as a bare billing API error; the command renders the
// shared sign-in guidance, including the `signup --force` path for a revoked
// key. fetchAccountSummary is the seam cmdAccount renders, so the test stays
// exit-free.
func TestAccount401Guidance(t *testing.T) {
	testConfigHome(t)
	fake := &fakeAccountAPI{err: parseBillingError(http.StatusUnauthorized, []byte(`{"detail":{"error":"unauthorized"}}`))}
	stubAccountClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := fetchAccountSummary()
	if err == nil {
		t.Fatal("fetchAccountSummary with a 401 = nil, want the API error")
	}
	if fake.calls != 1 {
		t.Fatalf("Account calls = %d, want 1", fake.calls)
	}
	msg := describeBillingError(err)
	if msg == err.Error() {
		t.Fatalf("401 rendered bare: %q", msg)
	}
	assertSignInGuidance(t, "account", msg)
	assertNoSecrets(t, msg, "tsk_secret")
}

// TestAccountSessionExpiryGuidance is the real-client end-to-end: a stored
// session the server rejects makes the command surface the client's re-login
// guidance, clear the dead session, and fall through to the stored agent key on
// the next run.
func TestAccountSessionExpiryGuidance(t *testing.T) {
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
		fmt.Fprint(w, accountServerJSON)
	}))
	defer srv.Close()

	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   srv.URL,
		BillingSession:  "dead-session",
		BillingAgentKey: "tsk_keep",
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := fetchAccountSummary()
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("fetchAccountSummary error = %v (%T), want errSessionExpired", err, err)
	}
	if !strings.Contains(err.Error(), "tessera login --email") {
		t.Errorf("expiry guidance = %q, want the login command", err)
	}
	if cookies[0] != "dead-session" || auths[0] != "" {
		t.Fatalf("first call sent cookie=%q auth=%q, want the stored session", cookies[0], auths[0])
	}
	saved, lerr := loadConfig()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if saved.BillingSession != "" {
		t.Fatalf("dead session not cleared: %q", saved.BillingSession)
	}
	if saved.BillingAgentKey != "tsk_keep" {
		t.Fatalf("agent key changed: %q", saved.BillingAgentKey)
	}

	// The next run uses the agent key and succeeds.
	acct, credential, err := fetchAccountSummary()
	if err != nil {
		t.Fatalf("fetchAccountSummary after expiry: %v", err)
	}
	if credential != "agent" {
		t.Fatalf("credential = %q, want agent", credential)
	}
	if acct["billing_id"] != "TESS-000000000042" {
		t.Fatalf("account = %#v", acct)
	}
	if len(auths) != 2 || auths[1] != "Bearer tsk_keep" || cookies[1] != "" {
		t.Fatalf("second call sent cookie=%q auth=%q, want the agent key", cookies[1], auths[1])
	}
}
