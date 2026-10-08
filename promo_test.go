package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePromoAPI implements billingAPI with only Promo wired. The embedded nil
// interface satisfies the other methods and panics if one is called, which is
// part of the assertion that promo speaks only to POST /billing/promo.
type fakePromoAPI struct {
	billingAPI
	calls int
	code  string
	resp  map[string]any
	err   error
}

func (f *fakePromoAPI) Promo(code string) (map[string]any, error) {
	f.calls++
	f.code = code
	return f.resp, f.err
}

// TestParsePromoArgs pins the single-positional contract: exactly one code,
// sent trimmed and otherwise untouched (moneyd owns normalization).
func TestParsePromoArgs(t *testing.T) {
	valid := []struct {
		name string
		args []string
		want string
	}{
		{"plain", []string{"WELCOME"}, "WELCOME"},
		{"lowercase kept as typed", []string{"welcome"}, "welcome"},
		{"surrounding whitespace trimmed", []string{"  WELCOME  "}, "WELCOME"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePromoArgs(tc.args)
			if err != nil {
				t.Fatalf("parsePromoArgs(%v) = %v, want nil", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parsePromoArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}

	invalid := []struct {
		name string
		args []string
	}{
		{"missing code", nil},
		{"empty code", []string{""}},
		{"whitespace-only code", []string{"   "}},
		{"two positionals", []string{"A", "B"}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePromoArgs(tc.args); err == nil {
				t.Fatalf("parsePromoArgs(%v) = nil error, want a usage error", tc.args)
			}
		})
	}
}

// TestCmdPromoCreditGrant drives a credit_usd redemption end-to-end through the
// fake: POST /billing/promo with the code, then kind and value printed with the
// null periods_remaining omitted (never rendered as a Go artifact) and no
// stored credential anywhere in the output.
func TestCmdPromoCreditGrant(t *testing.T) {
	testConfigHome(t)
	fake := &fakePromoAPI{resp: map[string]any{
		"promo_id":          float64(3),
		"kind":              "credit_usd",
		"value":             "7.5",
		"redemption_id":     float64(9),
		"credit_id":         float64(11),
		"periods_remaining": nil,
	}}
	stubBillingClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdPromo([]string{"welcome"}) })

	if fake.calls != 1 {
		t.Fatalf("Promo calls = %d, want 1", fake.calls)
	}
	if fake.code != "welcome" {
		t.Fatalf("Promo code = %q, want the code sent unchanged", fake.code)
	}
	for _, want := range []string{"credit_usd", "7.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("promo output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Periods") {
		t.Errorf("null periods_remaining was printed:\n%s", out)
	}
	if strings.Contains(out, "nil") || strings.Contains(out, "%!") || strings.Contains(out, "<nil>") {
		t.Errorf("null field rendered as a formatting artifact:\n%s", out)
	}
	assertNoSecrets(t, out, "tsk_secret")
}

// TestCmdPromoFreePeriods: a periods-carrying redemption prints the count.
func TestCmdPromoFreePeriods(t *testing.T) {
	testConfigHome(t)
	fake := &fakePromoAPI{resp: map[string]any{
		"kind":              "free_periods",
		"value":             "2",
		"periods_remaining": float64(2),
	}}
	stubBillingClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdPromo([]string{"DEAL"}) })
	if !strings.Contains(out, "free_periods") {
		t.Errorf("promo output missing the kind:\n%s", out)
	}
	if line := lineWithPrefix(out, "Periods remaining"); !strings.Contains(line, "2") {
		t.Errorf("periods line = %q, want the count 2", line)
	}
	assertNoSecrets(t, out, "tsk_secret")
}

// TestRunPromoErrorOutcomes: every failure shape moneyd emits (promos.py)
// returns a non-nil error with no success output, and describePromoError names
// the nested reason.
func TestRunPromoErrorOutcomes(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		want string
	}{
		{"404 promo_not_found", `{"detail":{"error":"promo_not_found"}}`, http.StatusNotFound, "promo_not_found"},
		{"409 promo_already_redeemed", `{"detail":{"error":"promo_already_redeemed"}}`, http.StatusConflict, "promo_already_redeemed"},
		{"409 promo_exhausted", `{"detail":{"error":"promo_exhausted"}}`, http.StatusConflict, "promo_exhausted"},
		{"409 promo_inactive", `{"detail":{"error":"promo_inactive"}}`, http.StatusConflict, "promo_inactive"},
		{"409 promo_new_accounts_only", `{"detail":{"error":"promo_new_accounts_only"}}`, http.StatusConflict, "promo_new_accounts_only"},
		{"409 no_active_subscription", `{"detail":{"error":"no_active_subscription"}}`, http.StatusConflict, "no_active_subscription"},
		{"409 promo_conflict", `{"detail":{"error":"promo_conflict"}}`, http.StatusConflict, "promo_conflict"},
		{"409 promo_invalid", `{"detail":{"error":"promo_invalid"}}`, http.StatusConflict, "promo_invalid"},
		{"401 bearer points at signup or login", `{"detail":{"error":"unauthorized"}}`, http.StatusUnauthorized, "tessera signup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePromoAPI{err: parseBillingError(tc.code, []byte(tc.body))}
			var runErr error
			out := captureStdout(t, func() {
				runErr = runPromo(fake, "CODE")
			})
			if runErr == nil {
				t.Fatal("runPromo = nil, want the API error returned")
			}
			if out != "" {
				t.Fatalf("a failed redemption printed success output:\n%s", out)
			}
			msg := describePromoError(runErr)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("guidance %q missing %q", msg, tc.want)
			}
			assertNoSecrets(t, msg, "tsk_secret", "sess-live")
		})
	}

	t.Run("plain error passes through", func(t *testing.T) {
		plain := fmt.Errorf("connection refused")
		if got := describePromoError(plain); got != plain.Error() {
			t.Fatalf("describePromoError = %q, want the original message", got)
		}
	})
}

// TestPromoClientGuidance: no config file and a config without billing
// credentials both fail before any API call, with the signup/login pointer.
func TestPromoClientGuidance(t *testing.T) {
	t.Run("no config file", func(t *testing.T) {
		testConfigHome(t)
		fake := &fakePromoAPI{}
		stubBillingClient(t, fake)

		_, err := promoClient()
		if err == nil {
			t.Fatal("promoClient with no config = nil, want guidance")
		}
		for _, want := range []string{"tessera signup", "tessera login"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		if fake.calls != 0 {
			t.Fatalf("Promo calls = %d, want 0 without credentials", fake.calls)
		}
	})

	t.Run("config without billing credentials", func(t *testing.T) {
		testConfigHome(t)
		if err := saveConfig(&Config{IndexerURL: defaultIndexer, AppID: "app-1"}); err != nil {
			t.Fatal(err)
		}
		fake := &fakePromoAPI{}
		stubBillingClient(t, fake)

		_, err := promoClient()
		if err == nil {
			t.Fatal("promoClient without credentials = nil, want guidance")
		}
		if fake.calls != 0 {
			t.Fatalf("Promo calls = %d, want 0 without credentials", fake.calls)
		}
	})
}

// TestPromoHTTPRequestShape captures what moneyd receives: POST
// /billing/promo with {"code": …} and the stored bearer key, and no secret in
// the printed result.
func TestPromoHTTPRequestShape(t *testing.T) {
	testConfigHome(t)
	var paths, methods, auths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		methods = append(methods, r.Method)
		auths = append(auths, r.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"promo_id":3,"kind":"free_periods","value":"2","redemption_id":9,"credit_id":null,"periods_remaining":2}`)
	}))
	defer srv.Close()

	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   srv.URL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdPromo([]string{" NEWYEAR "}) })

	if len(paths) != 1 || paths[0] != "/billing/promo" {
		t.Fatalf("paths = %v, want POST /billing/promo", paths)
	}
	if methods[0] != http.MethodPost {
		t.Fatalf("method = %q, want POST", methods[0])
	}
	if len(bodies) != 1 || bodies[0]["code"] != "NEWYEAR" {
		t.Fatalf("body = %#v, want the trimmed code", bodies[0])
	}
	if auths[0] != "Bearer tsk_secret" {
		t.Fatalf("Authorization = %q, want the stored bearer key", auths[0])
	}
	for _, want := range []string{"free_periods", "2"} {
		if !strings.Contains(out, want) {
			t.Errorf("promo output missing %q:\n%s", want, out)
		}
	}
	assertNoSecrets(t, out, "tsk_secret")
}
