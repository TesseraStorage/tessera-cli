package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTopupAPI implements billingAPI with only Topup wired. The embedded nil
// interface satisfies the other methods and panics if one is called, which is
// part of the assertion that topup speaks only to POST /billing/topup.
type fakeTopupAPI struct {
	billingAPI
	calls       int
	amount      string
	rail        string
	payCurrency string
	payment     string
	resp        map[string]any
	err         error
}

func (f *fakeTopupAPI) Topup(amount, rail, payCurrency, paymentHeader string) (map[string]any, error) {
	f.calls++
	f.amount, f.rail, f.payCurrency, f.payment = amount, rail, payCurrency, paymentHeader
	return f.resp, f.err
}

// testX402Header stands in for a base64 X-PAYMENT payload. It is fake key
// material: no test may find it echoed into command output.
const testX402Header = "eyJzaWduYXR1cmUiOiJzZWNyZXQtc2lnIn0="

// testX402Receipt stands in for the base64 PAYMENT-RESPONSE header value the
// server attaches to a 200 settlement; the CLI notes the header but never
// prints its contents.
const testX402Receipt = "eyJyZWNlaXB0Ijp0cnVlfQ=="

// x402ChallengeJSON mirrors moneyd's v2 PaymentRequired document
// (dataplane/moneyd/x402.py:payment_required plus the challenge_id and
// payment_intent_id stamps from payments.create_x402_challenge): the nonce
// lives in accepts[0].extra.x402ChallengeNonce and there is no validBefore.
const x402ChallengeJSON = `{
  "x402Version": 2,
  "resource": {
    "url": "https://tessera.storage/api/v1/account",
    "description": "Tessera prepaid balance top-up",
    "mimeType": "application/json"
  },
  "accepts": [
    {
      "scheme": "exact",
      "network": "eip155:84532",
      "amount": "5000000",
      "asset": "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
      "payTo": "0x1111111111111111111111111111111111111111",
      "maxTimeoutSeconds": 600,
      "extra": {
        "name": "USDC",
        "version": "2",
        "x402ChallengeNonce": "0x9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"
      }
    }
  ],
  "challenge_id": 7,
  "payment_intent_id": 3
}`

// assertNoSecrets fails when any of the given secrets appears in out. The
// topup command must never print stored credentials or X-PAYMENT contents.
func assertNoSecrets(t *testing.T, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(out, s) {
			t.Errorf("output leaked %q:\n%s", s, out)
		}
	}
}

// TestTopupValueFlagsRegistered guards the positional() trap: value flags
// such as --pay-currency must be known to takesValue so their arguments
// are not treated as positional amounts.
func TestTopupValueFlagsRegistered(t *testing.T) {
	for _, flag := range []string{"--pay-currency", "--coin", "--asset"} {
		if !takesValue(flag) {
			t.Errorf("takesValue(%q) = false; positional() would swallow its value", flag)
		}
	}
}

func TestValidateTopupAmountRange(t *testing.T) {
	for _, ok := range []string{"5", "12.50", "10000", "1e3", "5.", "5.009"} {
		if err := validateTopupAmount(ok); err != nil {
			t.Errorf("validateTopupAmount(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "0", "-1", "abc", "5usd", "NaN", "Inf", "+Inf", "-Inf", "0.01", ".5", "4.99", "4.999", "10000.01"} {
		if err := validateTopupAmount(bad); err == nil {
			t.Errorf("validateTopupAmount(%q) = nil, want an error", bad)
		}
	}
}

// TestParseTopupArgs pins flag resolution: the positional amount and both
// flag forms of --pay-currency/--asset.
func TestParseTopupArgs(t *testing.T) {
	valid := []struct {
		name string
		args []string
		want topupRequest
	}{
		{"amount only", []string{"12.50"}, topupRequest{amount: "12.50"}},
		{"space-form pay-currency", []string{"12.50", "--pay-currency", "btc"}, topupRequest{amount: "12.50", payCurrency: "btc"}},
		{"equals-form pay-currency", []string{"--pay-currency=btc", "12.50"}, topupRequest{amount: "12.50", payCurrency: "btc"}},
		{"invoice", []string{"12.50", "--invoice"}, topupRequest{amount: "12.50", invoice: true}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTopupArgs(tc.args)
			if err != nil {
				t.Fatalf("parseTopupArgs(%v) = %v, want nil", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parseTopupArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}

	invalid := []struct {
		name string
		args []string
	}{
		{"missing amount", nil},
		{"empty amount", []string{""}},
		{"zero amount", []string{"0"}},
		{"negative amount", []string{"-5"}},
		{"non-numeric amount", []string{"five"}},
		{"two positionals", []string{"5", "6"}},
		{"removed wallet flag", []string{"5", "--wallet"}},
		{"removed x402 flag", []string{"5", "--x402"}},
		{"removed payment flag", []string{"5", "--payment", "xxx"}},
		{"pay-currency without value", []string{"5", "--pay-currency"}},
		{"empty pay-currency", []string{"5", "--pay-currency="}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTopupArgs(tc.args); err == nil {
				t.Fatalf("parseTopupArgs(%v) = nil error, want a usage error", tc.args)
			}
		})
	}
}

// TestCmdTopupInvoice drives the NOWPayments path end-to-end through the fake:
// POST /billing/topup with rail nowpayments and the optional asset, and the
// 201 fields printed with no credential anywhere in the output.
func TestCmdTopupInvoice(t *testing.T) {
	testConfigHome(t)
	fake := &fakeTopupAPI{resp: map[string]any{
		"payment_intent_id": float64(12),
		"invoice_url":       "https://pay.example/i/abc",
		"expires_at":        "2026-10-08T13:00:00Z",
	}}
	stubBillingClient(t, fake)
	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdTopup([]string{"12.50", "--pay-currency", "btc"}) })

	if fake.calls != 1 {
		t.Fatalf("Topup calls = %d, want 1", fake.calls)
	}
	if fake.amount != "12.50" || fake.rail != "nowpayments" || fake.payCurrency != "btc" || fake.payment != "" {
		t.Fatalf("Topup(%q, %q, %q, %q), want (12.50, nowpayments, btc, \"\")",
			fake.amount, fake.rail, fake.payCurrency, fake.payment)
	}
	for _, want := range []string{"12", "https://pay.example/i/abc", time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC).In(time.Local).Format("2006-01-02 15:04"), "UTC", "any accepted coin"} {
		if !strings.Contains(out, want) {
			t.Errorf("invoice output missing %q:\n%s", want, out)
		}
	}
	assertNoSecrets(t, out, "tsk_secret")
}

// TestTopupInvoiceHTTPRequestShape captures what moneyd receives on the
// invoice path: POST /billing/topup, amount_usd as the raw string, the
// explicit nowpayments rail, pay_currency only when requested, and no
// X-PAYMENT header.
func TestTopupInvoiceHTTPRequestShape(t *testing.T) {
	testConfigHome(t)
	var paths []string
	var bodies []map[string]any
	var xpayments []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/billing/currencies" {
			fmt.Fprint(w, `[]`)
			return
		}
		if r.URL.Path == "/billing/topup/minimums" {
			// The public minimum pre-check (GET) is not part of the shape.
			fmt.Fprint(w, `{"rails":{"nowpayments":{"minimum_usd":"0.38","pay_currency":"usdcbase"}}}`)
			return
		}
		paths = append(paths, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		xpayments = append(xpayments, r.Header.Get("X-PAYMENT"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"payment_intent_id":12,"invoice_url":"https://pay.example/i/abc","expires_at":"2026-10-08T13:00:00Z"}`)
	}))
	defer srv.Close()

	if err := saveConfig(&Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   srv.URL,
		BillingAgentKey: "tsk_secret",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdTopup([]string{"12.50", "--pay-currency", "btc"}) })
	out2 := captureStdout(t, func() { cmdTopup([]string{"5"}) })

	if len(paths) != 2 || paths[0] != "/billing/topup" || paths[1] != "/billing/topup" {
		t.Fatalf("paths = %v, want POST /billing/topup twice", paths)
	}
	if bodies[0]["amount_usd"] != "12.50" || bodies[0]["rail"] != "nowpayments" || bodies[0]["pay_currency"] != "btc" {
		t.Errorf("body = %#v, want amount_usd, rail nowpayments and pay_currency btc", bodies[0])
	}
	if len(bodies[1]) != 2 || bodies[1]["amount_usd"] != "5" || bodies[1]["rail"] != "nowpayments" {
		t.Errorf("amount-only body = %#v, want only amount_usd and the nowpayments rail", bodies[1])
	}
	if xpayments[0] != "" || xpayments[1] != "" {
		t.Errorf("invoice path sent X-PAYMENT: %q / %q", xpayments[0], xpayments[1])
	}
	for _, o := range []string{out, out2} {
		assertNoSecrets(t, o, "tsk_secret", testX402Header)
	}
}

func TestRunTopupErrorOutcomes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "409 below_asset_minimum keeps the minimum",
			err:  parseBillingError(http.StatusConflict, []byte(`{"error":"below_asset_minimum","minimum_usd":"10.00"}`)),
			want: []string{"below_asset_minimum", "$10.00", "another coin", "--invoice"},
		},
		{
			name: "422 amount_out_of_range names the $5 to $10,000 range",
			err:  parseBillingError(http.StatusUnprocessableEntity, []byte(`{"detail":{"error":"amount_out_of_range"}}`)),
			want: []string{"amount_out_of_range", "between $5 and $10,000"},
		},
		{
			name: "422 unknown_rail",
			err:  parseBillingError(http.StatusUnprocessableEntity, []byte(`{"detail":{"error":"unknown_rail"}}`)),
			want: []string{"unknown_rail"},
		},
		{
			name: "503 payments_unconfigured",
			err:  parseBillingError(http.StatusServiceUnavailable, []byte(`{"detail":{"error":"payments_unconfigured"}}`)),
			want: []string{"payments_unconfigured"},
		},
		{
			name: "502 settlement_pending keeps the transaction and a retry hint",
			err:  parseBillingError(http.StatusBadGateway, []byte(`{"error":"settlement_pending","transaction":"0xabc123"}`)),
			want: []string{"settlement_pending", "0xabc123", "retry"},
		},
		{
			name: "502 processor_error",
			err:  parseBillingError(http.StatusBadGateway, []byte(`{"detail":{"error":"processor_error"}}`)),
			want: []string{"processor_error"},
		},
		{
			name: "402 refusal prints the error code",
			err:  parseBillingError(http.StatusPaymentRequired, []byte(`{"error":"invalid_payload"}`)),
			want: []string{"invalid_payload"},
		},
		{
			name: "401 bearer points at signup or login",
			err:  parseBillingError(http.StatusUnauthorized, []byte(`{"detail":{"error":"unauthorized"}}`)),
			want: []string{"401", "tessera signup", "tessera login"},
		},
		{
			name: "expired session keeps the client's guidance",
			err:  errSessionExpired,
			want: []string{"session expired", "tessera login --code"},
		},
		{
			name: "no credentials keeps the client's guidance",
			err:  errNoBillingCredentials,
			want: []string{"tessera signup", "tessera login"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTopupAPI{err: tc.err}
			var runErr error
			out := captureStdout(t, func() {
				runErr = runTopup(fake, topupRequest{amount: "5"})
			})
			if runErr == nil {
				t.Fatal("runTopup = nil, want the API error returned")
			}
			if out != "" {
				t.Fatalf("a failed topup printed success output:\n%s", out)
			}
			msg := describeTopupError(runErr)
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("guidance %q missing %q", msg, want)
				}
			}
			assertNoSecrets(t, msg, "tsk_secret", "sess-live", testX402Header)
		})
	}
}

func TestTopup401SignInGuidance(t *testing.T) {
	err := parseBillingError(http.StatusUnauthorized, []byte(`{"detail":{"error":"unauthorized"}}`))
	assertSignInGuidance(t, "topup", describeTopupError(err))
}

func TestTopupClientGuidance(t *testing.T) {
	t.Run("no config file", func(t *testing.T) {
		testConfigHome(t)
		fake := &fakeTopupAPI{}
		stubBillingClient(t, fake)

		_, err := topupClient()
		if !errors.Is(err, errNoBillingCredentials) {
			t.Fatalf("topupClient error = %v (%T), want errNoBillingCredentials", err, err)
		}
		for _, want := range []string{"tessera signup", "tessera login"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		if fake.calls != 0 {
			t.Fatalf("Topup calls = %d, want 0 without credentials", fake.calls)
		}
	})

	t.Run("config without billing credentials", func(t *testing.T) {
		testConfigHome(t)
		if err := saveConfig(&Config{IndexerURL: defaultIndexer, AppID: "app-1"}); err != nil {
			t.Fatal(err)
		}
		fake := &fakeTopupAPI{}
		stubBillingClient(t, fake)

		_, err := topupClient()
		if !errors.Is(err, errNoBillingCredentials) {
			t.Fatalf("topupClient error = %v (%T), want errNoBillingCredentials", err, err)
		}
		if fake.calls != 0 {
			t.Fatalf("Topup calls = %d, want 0 without credentials", fake.calls)
		}
	})
}
