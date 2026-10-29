package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeMinimumsAPI struct {
	fakeDirectAPI
	doc       map[string]any
	err       error
	askedCoin []string
}

func (f *fakeMinimumsAPI) TopupMinimums(coin string) (map[string]any, error) {
	f.askedCoin = append(f.askedCoin, coin)
	return f.doc, f.err
}

func minimumsDoc(min, coin string) map[string]any {
	return map[string]any{"rails": map[string]any{"nowpayments": map[string]any{"minimum_usd": min, "pay_currency": coin}}}
}

func TestCheckTopupMinimumRefusesBeforeInvoice(t *testing.T) {
	api := &fakeMinimumsAPI{doc: minimumsDoc("0.38", "usdcbase")}
	err := checkTopupMinimum(api, topupRequest{amount: "0.01", invoice: true, payCurrency: "btc"})
	var be *billingError
	if !errors.As(err, &be) || be.Status != http.StatusConflict || be.Code != "below_asset_minimum" {
		t.Fatalf("err = %v, want 409 below_asset_minimum", err)
	}
	if api.askedCoin[0] != "" {
		t.Fatalf("invoice should check the default coin, asked %q", api.askedCoin[0])
	}
	msg := describeTopupError(err)
	for _, want := range []string{"USDC on Base", "browser invoice", "$0.38", "nothing was created", "--wallet"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	if api.topupCalls != 0 {
		t.Fatal("no invoice may be created")
	}
}

func TestCheckTopupMinimumAllowsAtOrAbove(t *testing.T) {
	api := &fakeMinimumsAPI{doc: minimumsDoc("0.38", "usdcbase")}
	for _, amt := range []string{"0.38", "5"} {
		if err := checkTopupMinimum(api, topupRequest{amount: amt}); err != nil {
			t.Fatalf("%s: %v", amt, err)
		}
	}
	api2 := &fakeMinimumsAPI{doc: minimumsDoc("1.62", "btc")}
	if err := checkTopupMinimum(api2, topupRequest{amount: "1", payCurrency: "btc"}); err == nil {
		t.Fatal("btc $1 should be below $1.62")
	}
	if api2.askedCoin[0] != "btc" {
		t.Fatalf("direct should ask for the named coin, got %q", api2.askedCoin[0])
	}
}

func TestCheckTopupMinimumOutageDefersToServer(t *testing.T) {
	api := &fakeMinimumsAPI{err: errors.New("down")}
	if err := checkTopupMinimum(api, topupRequest{amount: "0.01"}); err != nil {
		t.Fatal(err)
	}
	api = &fakeMinimumsAPI{doc: map[string]any{"rails": map[string]any{"nowpayments": map[string]any{"minimum_usd": nil}}}}
	if err := checkTopupMinimum(api, topupRequest{amount: "0.01"}); err != nil {
		t.Fatal(err)
	}
}

func TestAssetLabelPlainWords(t *testing.T) {
	cases := map[[2]string]string{
		{"usdcbase", "base"}: "USDC on Base", {"usdttrc20", "trx"}: "USDT on Tron",
		{"xmr", "xmr"}: "Monero (XMR)", {"foocoin", "bsc"}: "FOOCOIN on BNB Chain", {"zzz", "zzz"}: "ZZZ",
	}
	for in, want := range cases {
		if got := assetLabel(in[0], in[1]); got != want {
			t.Errorf("assetLabel(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalTimeLabelNamesZoneAndRelative(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("ICT", 7*3600)
	defer func() { time.Local = old }()
	now := time.Date(2026, 10, 30, 18, 53, 0, 0, time.UTC)
	got := localTimeLabel("2026-10-30T19:13:00Z", now)
	if got != "2026-10-31 02:13 UTC+07:00 (in 20 min)" {
		t.Fatalf("got %q", got)
	}
	if got := localTimeLabel("2026-10-30T18:00:00.123Z", now); !strings.Contains(got, "expired 53 min ago") {
		t.Fatalf("past label %q", got)
	}
}

func TestResolveAssetTickerListsNetworks(t *testing.T) {
	api := &fakeMinimumsAPI{}
	api.currenciesResp = []map[string]any{{"code": "usdttrc20", "name": "Tether USD (Tron)"}, {"code": "usdtbsc", "name": "Tether USD (BSC)"}, {"code": "xmr", "name": "Monero"}}
	if c, err := resolveAssetCode(api, "xmr"); err != nil || c != "xmr" {
		t.Fatalf("xmr -> %q %v", c, err)
	}
	_, err := resolveAssetCode(api, "usdt")
	if err == nil || !strings.Contains(err.Error(), "usdttrc20") || !strings.Contains(err.Error(), "usdtbsc") {
		t.Fatalf("usdt err = %v", err)
	}
	if _, err := resolveAssetCode(api, "nope"); err == nil {
		t.Fatal("unknown coin should be refused")
	}
}

func TestTopupHelpExplainsThreeWays(t *testing.T) {
	for _, want := range []string{"x402 wallet", "any amount from $0.01", "--asset xmr", "Browser invoice", "XMR", "BTC", "ETH", "USDT"} {
		if !strings.Contains(topupHelp, want) {
			t.Errorf("topup help missing %q", want)
		}
	}
}
