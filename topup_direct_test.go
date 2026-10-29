package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// fakeDirectAPI implements every optional billing capability so the default,
// polling, coin-list and wallet paths can be exercised without a server.
type fakeDirectAPI struct {
	billingAPI
	topupCalls    int
	topupAmount   string
	topupRail     string
	topupCurrency string
	topupPayment  string
	topupMode     string
	topupResp     map[string]any
	topupErr      error

	statusCalls int
	statusResp  map[string]any
	statusErr   error

	accountResp map[string]any
	accountErr  error

	challengeResp map[string]any
	challengeErr  error

	currenciesResp []map[string]any
	currenciesErr  error
}

func (f *fakeDirectAPI) Topup(amount, rail, payCurrency, paymentHeader string) (map[string]any, error) {
	return f.TopupWithMode(amount, rail, payCurrency, paymentHeader, "")
}

func (f *fakeDirectAPI) TopupWithMode(amount, rail, payCurrency, paymentHeader, mode string) (map[string]any, error) {
	f.topupCalls++
	f.topupAmount, f.topupRail, f.topupCurrency, f.topupPayment, f.topupMode = amount, rail, payCurrency, paymentHeader, mode
	return f.topupResp, f.topupErr
}

func (f *fakeDirectAPI) TopupStatus(paymentIntentID string) (map[string]any, error) {
	f.statusCalls++
	return f.statusResp, f.statusErr
}

func (f *fakeDirectAPI) Account() (map[string]any, error) {
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	if f.accountResp != nil {
		return f.accountResp, nil
	}
	return map[string]any{"balance_usd": "20.00", "topup_rails": map[string]any{"x402": false}}, nil
}

func (f *fakeDirectAPI) X402Challenge(amount string) (map[string]any, error) {
	return f.challengeResp, f.challengeErr
}

func (f *fakeDirectAPI) Currencies() ([]map[string]any, error) {
	return f.currenciesResp, f.currenciesErr
}

func directTopupResponse() map[string]any {
	return map[string]any{
		"payment_intent_id":  float64(21),
		"invoice_url":        "https://pay.example/i/direct",
		"expires_at":         "2026-10-28T12:00:00Z",
		"status":             "pending",
		"direct_payment":     true,
		"pay_address":        "0x52908400098527886E0F7030069857D2E4169EE7",
		"pay_amount":         "10.199555",
		"pay_currency":       "usdcbase",
		"network":            "base",
		"address_expires_at": "2026-10-28T11:55:00Z",
	}
}

func stubPolling(t *testing.T) {
	t.Helper()
	oldInitial, oldMax, oldTimeout := topupPollInitialInterval, topupPollMaxInterval, topupPollTimeout
	topupPollInitialInterval = time.Millisecond
	topupPollMaxInterval = time.Millisecond
	topupPollTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		topupPollInitialInterval, topupPollMaxInterval, topupPollTimeout = oldInitial, oldMax, oldTimeout
	})
}

func TestParseTopupDirectFlags(t *testing.T) {
	valid := []struct {
		name string
		args []string
		want topupRequest
	}{
		{"coin", []string{"12.50", "--coin", "btc"}, topupRequest{amount: "12.50", payCurrency: "btc"}},
		{"invoice", []string{"12.50", "--invoice"}, topupRequest{amount: "12.50", invoice: true}},
		{"wallet", []string{"12.50", "--wallet"}, topupRequest{amount: "12.50", wallet: true}},
		{"json no-wait", []string{"12.50", "--json", "--no-wait"}, topupRequest{amount: "12.50", json: true, noWait: true}},
		{"wait", []string{"12.50", "--wait"}, topupRequest{amount: "12.50", wait: true}},
		{"coins", []string{"--coins"}, topupRequest{coins: true}},
		{"coins json", []string{"--coins", "--json"}, topupRequest{coins: true, json: true}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTopupArgs(tc.args)
			if err != nil {
				t.Fatalf("parseTopupArgs(%v) = %v", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parseTopupArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
	invalid := [][]string{
		{"12.50", "--wallet", "--x402"},
		{"12.50", "--wallet", "--invoice"},
		{"12.50", "--invoice", "--payment", testX402Header},
		{"12.50", "--wait", "--no-wait"},
		{"12.50", "--x402", "--coin", "btc"},
		{"12.50", "--coin", "btc", "--pay-currency", "eth"},
	}
	for _, args := range invalid {
		if _, err := parseTopupArgs(args); err == nil {
			t.Fatalf("parseTopupArgs(%v) = nil error, want a conflict error", args)
		}
	}
}

func TestRunTopupAddressDirect(t *testing.T) {
	stubStdinTerminal(t, false)
	fake := &fakeDirectAPI{topupResp: directTopupResponse()}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10"}); err != nil {
			t.Fatal(err)
		}
	})
	if fake.topupCalls != 1 || fake.topupRail != "nowpayments" || fake.topupMode != "" {
		t.Fatalf("TopupWithMode calls=%d rail=%q mode=%q, want 1/nowpayments/empty", fake.topupCalls, fake.topupRail, fake.topupMode)
	}
	if fake.statusCalls != 0 {
		t.Fatalf("non-interactive default path polled %d times, want 0", fake.statusCalls)
	}
	for _, want := range []string{"Top-up address created", "10.199555", "USDC on Base", "Send before:", "UTC", "0x52908400098527886E0F7030069857D2E4169EE7", "https://pay.example/i/direct"} {
		if !strings.Contains(out, want) {
			t.Errorf("direct output missing %q:\n%s", want, out)
		}
	}
}

func TestRunTopupAddressInvoice(t *testing.T) {
	fake := &fakeDirectAPI{topupResp: map[string]any{
		"payment_intent_id": float64(22),
		"invoice_url":       "https://pay.example/i/invoice",
		"expires_at":        "2026-10-28T12:00:00Z",
	}}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10", invoice: true}); err != nil {
			t.Fatal(err)
		}
	})
	if fake.topupMode != "invoice" {
		t.Fatalf("mode = %q, want invoice", fake.topupMode)
	}
	for _, want := range []string{"Top-up invoice created", "https://pay.example/i/invoice"} {
		if !strings.Contains(out, want) {
			t.Errorf("invoice output missing %q:\n%s", want, out)
		}
	}
}

func TestRunTopupAddressCoin(t *testing.T) {
	fake := &fakeDirectAPI{topupResp: directTopupResponse()}
	_ = captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10", payCurrency: "btc"}); err != nil {
			t.Fatal(err)
		}
	})
	if fake.topupCurrency != "btc" {
		t.Fatalf("payCurrency = %q, want btc", fake.topupCurrency)
	}
}

func TestRunTopupOldServerFallback(t *testing.T) {
	stubStdinTerminal(t, false)
	fake := &fakeTopupAPI{resp: map[string]any{
		"payment_intent_id": float64(12),
		"invoice_url":       "https://pay.example/i/old",
		"expires_at":        "2026-10-08T13:00:00Z",
	}}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "5"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Top-up invoice created") {
		t.Fatalf("fallback did not print the invoice:\n%s", out)
	}
	if strings.Contains(out, "Top-up address created") {
		t.Fatalf("fallback printed a direct address for an invoice-only response:\n%s", out)
	}
}

func TestRunTopupJSONCreate(t *testing.T) {
	fake := &fakeDirectAPI{topupResp: directTopupResponse()}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10", json: true}); err != nil {
			t.Fatal(err)
		}
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not one JSON object: %v\n%s", err, out)
	}
	for _, key := range []string{"payment_intent_id", "invoice_url", "pay_address", "pay_amount", "pay_currency", "network", "amount_usd"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("JSON create output missing %q: %v", key, doc)
		}
	}
	if doc["amount_usd"] != "10" {
		t.Errorf("amount_usd = %v, want 10", doc["amount_usd"])
	}
}

func TestRunTopupPollsToCompleted(t *testing.T) {
	stubPolling(t)
	stubStdinTerminal(t, false)
	fake := &fakeDirectAPI{
		topupResp:  directTopupResponse(),
		statusResp: map[string]any{"status": "completed", "credited_usd": "10.00"},
		accountResp: map[string]any{
			"balance_usd": "30.00",
			"topup_rails": map[string]any{"x402": false},
		},
	}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10", wait: true}); err != nil {
			t.Fatal(err)
		}
	})
	if fake.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1", fake.statusCalls)
	}
	for _, want := range []string{"Credited $10.00", "balance $30.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("completed output missing %q:\n%s", want, out)
		}
	}
}

func TestRunTopupJSONCompleted(t *testing.T) {
	stubPolling(t)
	fake := &fakeDirectAPI{
		topupResp:   directTopupResponse(),
		statusResp:  map[string]any{"status": "completed", "credited_usd": "10.00"},
		accountResp: map[string]any{"balance_usd": "30.00", "topup_rails": map[string]any{"x402": false}},
	}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10", json: true, wait: true}); err != nil {
			t.Fatal(err)
		}
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json completed output is not JSON: %v\n%s", err, out)
	}
	if doc["status"] != "completed" || doc["credited_usd"] != "10.00" || doc["balance_usd"] != "30.00" {
		t.Fatalf("completed JSON = %v", doc)
	}
	if doc["pay_address"] != "0x52908400098527886E0F7030069857D2E4169EE7" {
		t.Fatalf("completed JSON lost the create fields: %v", doc)
	}
}

func TestRunTopupPollExpired(t *testing.T) {
	stubPolling(t)
	fake := &fakeDirectAPI{
		topupResp:  directTopupResponse(),
		statusResp: map[string]any{"status": "expired"},
	}
	err := runTopup(fake, topupRequest{amount: "10", wait: true})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("runTopup = %v, want an expired error", err)
	}
}

func TestRunTopupPollTimeout(t *testing.T) {
	stubPolling(t)
	old := topupPollTimeout
	topupPollTimeout = 2 * time.Millisecond
	t.Cleanup(func() { topupPollTimeout = old })
	fake := &fakeDirectAPI{
		topupResp:  directTopupResponse(),
		statusResp: map[string]any{"status": "pending"},
	}
	var err error
	out := captureStdout(t, func() { err = runTopup(fake, topupRequest{amount: "10", wait: true}) })
	if err != nil {
		t.Fatalf("runTopup timeout = %v, want nil (still pending)", err)
	}
	if !strings.Contains(out, "Still pending") {
		t.Errorf("timeout output missing 'Still pending':\n%s", out)
	}
}

func TestRunTopupCoins(t *testing.T) {
	fake := &fakeDirectAPI{currenciesResp: []map[string]any{
		{"code": "usdcbase", "name": "USD Coin (Base)", "default": true},
		{"code": "btc", "name": "Bitcoin"},
	}}
	stubBillingClient(t, fake)
	out := captureStdout(t, func() {
		if err := runTopupCoins(topupRequest{}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"usdcbase", "USD Coin (Base)", "btc", "Bitcoin"} {
		if !strings.Contains(out, want) {
			t.Errorf("coin output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "* usdcbase") {
		t.Errorf("coin output did not mark the default:\n%s", out)
	}
}

func TestRunTopupCoinsJSON(t *testing.T) {
	fake := &fakeDirectAPI{currenciesResp: []map[string]any{
		{"code": "usdcbase", "name": "USD Coin (Base)", "default": true},
	}}
	stubBillingClient(t, fake)
	out := captureStdout(t, func() {
		if err := runTopupCoins(topupRequest{json: true}); err != nil {
			t.Fatal(err)
		}
	})
	var docs []map[string]any
	if err := json.Unmarshal([]byte(out), &docs); err != nil {
		t.Fatalf("coin JSON output is not an array: %v\n%s", err, out)
	}
	if len(docs) != 1 || docs[0]["code"] != "usdcbase" {
		t.Fatalf("coin JSON = %v", docs)
	}
}

func TestWalletInitPermissionsAndNoKeyPrint(t *testing.T) {
	home := testConfigHome(t)
	out := captureStdout(t, func() {
		if err := runWalletInit(nil); err != nil {
			t.Fatal(err)
		}
	})
	path := filepath.Join(home, walletFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("wallet file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("wallet mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc walletKey
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, doc.PrivateKey) {
		t.Fatalf("wallet init printed the private key:\n%s", out)
	}
	for _, want := range []string{"Wallet created", doc.Address, "You control the key; back it up"} {
		if !strings.Contains(out, want) {
			t.Errorf("wallet init output missing %q:\n%s", want, out)
		}
	}
	if err := runWalletInit(nil); err == nil {
		t.Fatal("second wallet init succeeded, want an already-exists error")
	}
}

func TestWalletShowRPCBalance(t *testing.T) {
	home := testConfigHome(t)
	if err := runWalletInit(nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, walletFileName)
	data, _ := os.ReadFile(path)
	var wallet walletKey
	_ = json.Unmarshal(data, &wallet)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_chainId":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0x2105"}`)
		case "eth_call":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0xbc614e"}`)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"message":"unexpected method"}}`)
		}
	}))
	defer srv.Close()
	t.Setenv("TESSERA_WALLET_RPC_URL", srv.URL)
	out := captureStdout(t, func() {
		if err := runWalletShow(nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{wallet.Address, "USDC on Base: $12.345678"} {
		if !strings.Contains(out, want) {
			t.Errorf("wallet show output missing %q:\n%s", want, out)
		}
	}
}

func TestWalletShowUnknownChain(t *testing.T) {
	testConfigHome(t)
	if err := runWalletInit(nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	}))
	defer srv.Close()
	t.Setenv("TESSERA_WALLET_RPC_URL", srv.URL)
	_, stderr := captureOutput(t, func() {
		if err := runWalletShow(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "USDC balance unavailable on chain 1") {
		t.Errorf("unknown-chain stderr = %q", stderr)
	}
}

func TestSignX402PaymentVerifies(t *testing.T) {
	key, err := newWalletKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := walletPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	challenge := x402ChallengeMap(t)
	now := time.Unix(1700000000, 0)
	header, err := signX402Payment(challenge, priv, now)
	if err != nil {
		t.Fatalf("signX402Payment: %v", err)
	}
	payload := decodePaymentHeader(t, header)
	if payload["x402Version"] != float64(2) {
		t.Fatalf("x402Version = %v", payload["x402Version"])
	}
	accepted, ok := payload["accepted"].(map[string]any)
	if !ok {
		t.Fatalf("payload accepted = %T", payload["accepted"])
	}
	if topupField(accepted, "network") != "eip155:84532" {
		t.Fatalf("accepted network = %v", accepted["network"])
	}
	inner, ok := payload["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload.payload = %T", payload["payload"])
	}
	sigHex, _ := inner["signature"].(string)
	if !strings.HasPrefix(sigHex, "0x") {
		t.Fatalf("signature = %q", sigHex)
	}
	auth, ok := inner["authorization"].(map[string]any)
	if !ok {
		t.Fatalf("authorization = %T", inner["authorization"])
	}
	if auth["from"] != key.Address {
		t.Errorf("authorization from = %v, want %s", auth["from"], key.Address)
	}
	if auth["to"] != "0x1111111111111111111111111111111111111111" {
		t.Errorf("authorization to = %v", auth["to"])
	}
	if auth["value"] != "5000000" {
		t.Errorf("authorization value = %v", auth["value"])
	}
	if auth["nonce"] != "0x9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0" {
		t.Errorf("authorization nonce = %v", auth["nonce"])
	}

	// Rebuild the digest from the signed fields and prove the signature
	// recovers the wallet's public key. This is the same check the token and
	// the server's verifier perform.
	asset, _ := addressBytes("0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	payTo, _ := addressBytes("0x1111111111111111111111111111111111111111")
	from, _ := addressBytes(key.Address)
	nonce, _ := decodeHex("0x9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0")
	validBefore, _ := new(big.Int).SetString(topupField(auth, "validBefore"), 10)
	domain := eip712DomainSeparator("USDC", "2", 84532, asset)
	digest := transferAuthorizationDigest(domain, from, payTo, big.NewInt(5000000), big.NewInt(0), validBefore, nonce)
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil || len(sig) != 65 {
		t.Fatalf("signature bytes: %v len=%d", err, len(sig))
	}
	compact := make([]byte, 65)
	compact[0] = sig[64]
	copy(compact[1:33], sig[:32])
	copy(compact[33:], sig[32:64])
	pub, _, err := ecdsa.RecoverCompact(compact, digest)
	if err != nil {
		t.Fatalf("RecoverCompact: %v", err)
	}
	if !pub.IsEqual(priv.PubKey()) {
		t.Fatal("signature did not recover the wallet public key")
	}
}

func TestSignX402PaymentRejectsBadChallenge(t *testing.T) {
	key, _ := newWalletKey()
	priv, _ := walletPrivateKey(key)
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing accepts", func(m map[string]any) { delete(m, "accepts") }},
		{"bad network", func(m map[string]any) { firstAccept(m)["network"] = "solana" }},
		{"bad asset", func(m map[string]any) { firstAccept(m)["asset"] = "0x1234" }},
		{"bad amount", func(m map[string]any) { firstAccept(m)["amount"] = "0" }},
		{"short nonce", func(m map[string]any) {
			firstAccept(m)["extra"].(map[string]any)["x402ChallengeNonce"] = "0xabcd"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			challenge := x402ChallengeMap(t)
			tc.mutate(challenge)
			if _, err := signX402Payment(challenge, priv, now); err == nil {
				t.Fatal("signX402Payment succeeded, want an error")
			}
		})
	}
}

func TestRunWalletTopupSignsAndSettles(t *testing.T) {
	testConfigHome(t)
	if err := runWalletInit(nil); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDirectAPI{
		accountResp:   map[string]any{"topup_rails": map[string]any{"x402": true}},
		challengeResp: x402ChallengeMap(t),
		topupResp: map[string]any{
			"balance_usd": "15.00",
			"transaction": "0xabc",
			"network":     "base-sepolia",
		},
	}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "5", wallet: true}); err != nil {
			t.Fatal(err)
		}
	})
	if fake.topupCalls != 1 || fake.topupRail != "x402" || fake.topupPayment == "" {
		t.Fatalf("TopupWithMode calls=%d rail=%q payment=%q", fake.topupCalls, fake.topupRail, fake.topupPayment)
	}
	payload := decodePaymentHeader(t, fake.topupPayment)
	inner := payload["payload"].(map[string]any)
	auth := inner["authorization"].(map[string]any)
	if auth["value"] != "5000000" {
		t.Errorf("signed value = %v, want 5000000", auth["value"])
	}
	if !strings.Contains(out, "x402 payment settled") {
		t.Errorf("wallet output missing settlement notice:\n%s", out)
	}
}

func TestRunWalletTopupClosedRail(t *testing.T) {
	testConfigHome(t)
	if err := runWalletInit(nil); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDirectAPI{
		accountResp: map[string]any{"topup_rails": map[string]any{"x402": false}},
	}
	err := runTopup(fake, topupRequest{amount: "5", wallet: true})
	if err == nil || !strings.Contains(err.Error(), "x402 is closed") {
		t.Fatalf("runTopup = %v, want the closed-rail message", err)
	}
	if fake.topupCalls != 0 {
		t.Fatalf("closed rail still called Topup %d times", fake.topupCalls)
	}
}

func TestRunWalletTopupNoWallet(t *testing.T) {
	testConfigHome(t)
	fake := &fakeDirectAPI{
		accountResp: map[string]any{"topup_rails": map[string]any{"x402": true}},
	}
	err := runTopup(fake, topupRequest{amount: "5", wallet: true})
	if err == nil || !strings.Contains(err.Error(), "tessera wallet init") {
		t.Fatalf("runTopup = %v, want the init guidance", err)
	}
}

func TestChainIDFromNetwork(t *testing.T) {
	cases := map[string]uint64{
		"base":         8453,
		"base-mainnet": 8453,
		"base-sepolia": 84532,
		"eip155:8453":  8453,
		"eip155:84532": 84532,
	}
	for network, want := range cases {
		got, err := chainIDFromNetwork(network)
		if err != nil || got != want {
			t.Errorf("chainIDFromNetwork(%q) = %d, %v; want %d", network, got, err, want)
		}
	}
	if _, err := chainIDFromNetwork("solana"); err == nil {
		t.Error("chainIDFromNetwork(solana) = nil error, want unsupported")
	}
}

func TestFormatUSDC(t *testing.T) {
	cases := map[string]*big.Int{
		"0.000000":  big.NewInt(0),
		"0.000001":  big.NewInt(1),
		"12.345678": big.NewInt(12345678),
	}
	for want, atomic := range cases {
		if got := formatUSDC(atomic); got != want {
			t.Errorf("formatUSDC(%v) = %q, want %q", atomic, got, want)
		}
	}
}

// x402ChallengeMap parses the shared challenge JSON into the map shape the
// server sends.
func x402ChallengeMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(x402ChallengeJSON), &m); err != nil {
		t.Fatalf("challenge JSON: %v", err)
	}
	return m
}

func firstAccept(m map[string]any) map[string]any {
	accepts := m["accepts"].([]any)
	return accepts[0].(map[string]any)
}

// decodePaymentHeader base64-decodes an x402 header and returns its JSON
// object. A malformed header fails the test.
func decodePaymentHeader(t *testing.T, header string) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header is not base64: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("header is not JSON: %v\n%s", err, raw)
	}
	return doc
}

// Ensure the fake satisfies the optional interfaces the production paths use.
var (
	_ topupModeAPI     = (*fakeDirectAPI)(nil)
	_ topupStatusAPI   = (*fakeDirectAPI)(nil)
	_ currencyAPI      = (*fakeDirectAPI)(nil)
	_ x402ChallengeAPI = (*fakeDirectAPI)(nil)
)

func TestBelowMinimumGuidanceMentionsAmountAndX402(t *testing.T) {
	err := &billingError{
		Status: http.StatusConflict,
		Code:   "below_asset_minimum",
		Extra:  map[string]any{"minimum_usd": "8.64"},
	}
	msg := describeTopupError(err)
	for _, want := range []string{"below_asset_minimum", "$8.64", "nothing was created", "tessera wallet init", "--wallet", "--payment <base64 X-PAYMENT>", "$0.01"} {
		if !strings.Contains(msg, want) {
			t.Errorf("describeTopupError = %q, missing %q", msg, want)
		}
	}
}

func TestEmitTopupErrorJSONCarriesTheMinimum(t *testing.T) {
	out := captureStdout(t, func() {
		emitTopupErrorJSON(&billingError{
			Status: http.StatusConflict,
			Code:   "below_asset_minimum",
			Extra:  map[string]any{"minimum_usd": "8.64"},
		})
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("error output is not one JSON object: %v\n%s", err, out)
	}
	if doc["error"] != "below_asset_minimum" {
		t.Errorf("error = %v", doc["error"])
	}
	if doc["minimum_usd"] != "8.64" {
		t.Errorf("minimum_usd = %v", doc["minimum_usd"])
	}
	if doc["status"] != float64(http.StatusConflict) {
		t.Errorf("status = %v", doc["status"])
	}
	if !strings.Contains(topupField(doc, "message"), "$8.64") {
		t.Errorf("message = %v", doc["message"])
	}
}

func TestEmitTopupErrorJSONForANonBillingError(t *testing.T) {
	out := captureStdout(t, func() {
		emitTopupErrorJSON(errors.New("dial tcp: connection refused"))
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("error output is not JSON: %v\n%s", err, out)
	}
	if doc["error"] != "client_error" {
		t.Errorf("error = %v", doc["error"])
	}
	if !strings.Contains(topupField(doc, "message"), "connection refused") {
		t.Errorf("message = %v", doc["message"])
	}
}
