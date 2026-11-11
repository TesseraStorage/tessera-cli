package main

import (
	"strings"
	"testing"
)

func TestTokenBaseUnits(t *testing.T) {
	cases := []struct {
		in       string
		decimals int
		want     string
		ok       bool
	}{
		{"1", 18, "1000000000000000000", true},
		{"0.001", 18, "1000000000000000", true},
		{"0.1", 18, "100000000000000000", true},
		{"1.00", 6, "1000000", true},
		{"10.199555", 6, "10199555", true},
		{"0.0000005", 6, "1", true},
		{"0.0000004", 6, "0", true},
		{"1.23456789", 8, "123456789", true},
		{"1.234567895", 8, "123456790", true},
		{"1e18", 18, "", false},
		{"-1", 18, "", false},
		{"", 18, "", false},
	}
	for _, tc := range cases {
		got, err := tokenBaseUnits(tc.in, tc.decimals)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("tokenBaseUnits(%q, %d) = %q, %v; want %q", tc.in, tc.decimals, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("tokenBaseUnits(%q, %d) = %q, want an error", tc.in, tc.decimals, got)
		}
	}
}

func TestPassThroughDecimalNoFloat(t *testing.T) {
	got, err := passThroughDecimal("0.014000")
	if err != nil || got != "0.014000" {
		t.Fatalf("passThroughDecimal kept the input digits: got %q (%v)", got, err)
	}
	got, err = passThroughDecimal("0.1")
	if err != nil || got != "0.1" {
		t.Fatalf("0.1 = %q (%v)", got, err)
	}
	if _, err := passThroughDecimal("1.2e-3"); err == nil {
		t.Fatal("scientific notation must be rejected")
	}
}

func TestDepositQRPayloadTypes(t *testing.T) {
	const evm = "0x52908400098527886E0F7030069857D2E4169EE7"
	const btcAddr = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	const xmrAddr = "4AdUndXHHZ6cfufTMvppY6JwXNouMBzSkbLYfpAV5Usx3skxNgYeYTRj5UzqtReoS44qo9mtmXCqY45DJ852K5Jv2684Rge"
	const bchBare = "qpm2qsznhks23z7629mms6s4cwef74vcwvy22gdx6a"
	cases := []struct {
		name, currency, address, amount, want string
	}{
		{"btc", "btc", btcAddr, "0.00123456", "bitcoin:" + btcAddr + "?amount=0.00123456"},
		{"btc no amount", "btc", btcAddr, "", "bitcoin:" + btcAddr},
		{"ltc", "ltc", "LTCADDR123", "1.5", "litecoin:LTCADDR123?amount=1.5"},
		{"doge", "doge", "DADDR", "12.34", "dogecoin:DADDR?amount=12.34"},
		{"bch bare", "bch", bchBare, "0.01", "bitcoincash:" + bchBare + "?amount=0.01"},
		{"bch prefixed", "bch", "bitcoincash:" + bchBare, "0.01", "bitcoincash:" + bchBare + "?amount=0.01"},
		{"bch uppercase prefix", "bch", "BITCOINCASH:" + bchBare, "0.02", "bitcoincash:" + bchBare + "?amount=0.02"},
		{"xmr", "xmr", xmrAddr, "0.123456789012", "monero:" + xmrAddr + "?tx_amount=0.123456789012"},
		{"eth", "eth", evm, "0.001", "ethereum:" + evm + "@1?value=1000000000000000"},
		{"eth 1", "eth", evm, "1", "ethereum:" + evm + "@1?value=1000000000000000000"},
		{
			"usdc ethereum", "usdc", evm, "10.199555",
			"ethereum:0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48@1/transfer?address=" + evm + "&uint256=10199555",
		},
		{
			"usdt ethereum", "usdterc20", evm, "5",
			"ethereum:0xdAC17F958D2ee523a2206206994597C13D831ec7@1/transfer?address=" + evm + "&uint256=5000000",
		},
		{
			"usdcarb", "usdcarb", evm, "1",
			"ethereum:0xaf88d065e77c8cC2239327C5EDb3A432268e5831@42161/transfer?address=" + evm + "&uint256=1000000",
		},
		{"trc20 address only", "usdttrc20", "TN3W4H6rK2ce4vX9YnFQHwKENnHjoxb3m9", "12.5", "TN3W4H6rK2ce4vX9YnFQHwKENnHjoxb3m9"},
		{"sol address only", "sol", "SoL11111111111111111111111111111111111111112", "1.25", "SoL11111111111111111111111111111111111111112"},
		{"xrp address only", "xrp", "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh", "10", "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh"},
		{"buff doge not dogecoin scheme", "dogecoin", "BUFFADDR", "1", "BUFFADDR"},
		{"usdtbsc no guessed contract", "usdtbsc", evm, "5", evm},
		{"usdcbsc no guessed contract", "usdcbsc", evm, "5", evm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := depositQRPayloadFor(depositQR{Address: tc.address, Amount: tc.amount, Currency: tc.currency})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("payload =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}

	// USDC on Base must stay on the original builder.
	base, err := depositQRPayloadFor(depositQR{Address: evm, Amount: "10.199555", Currency: "usdcbase"})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := depositQRPayload(evm, "10.199555", qrEIP681)
	if err != nil {
		t.Fatal(err)
	}
	if base != legacy {
		t.Fatalf("USDC-on-Base payload changed:\n%s\nvs\n%s", base, legacy)
	}

	plain, err := depositQRPayloadFor(depositQR{Address: btcAddr, Amount: "0.5", Currency: "btc", Style: qrAddress})
	if err != nil || plain != btcAddr {
		t.Fatalf("--qr=address = %q (%v)", plain, err)
	}
}

func TestOtherQRCaption(t *testing.T) {
	if got, want := otherQRCaption(true, "15:04"), "exact amount, before 15:04"; got != want {
		t.Fatalf("exact caption = %q", got)
	}
	if got, want := otherQRCaption(false, ""), "address only — enter the amount shown above"; got != want {
		t.Fatalf("address caption = %q", got)
	}
}

func TestExtraIDFromResponse(t *testing.T) {
	if extraIDFromResponse(map[string]any{"pay_address": "x"}) != "" {
		t.Fatal("missing extra id should be empty")
	}
	if got := extraIDFromResponse(map[string]any{"payin_extra_id": "123456"}); got != "123456" {
		t.Fatalf("payin_extra_id = %q", got)
	}
	if got := extraIDFromResponse(map[string]any{"destination_tag": 98765}); got != "98765" {
		t.Fatalf("destination_tag = %q", got)
	}
}

func TestTopupPrintsMemoAndNonUSDCQR(t *testing.T) {
	stubQRTerminal(t, true, 120)

	btc := directTopupResponse()
	btc["pay_currency"] = "btc"
	btc["network"] = "btc"
	btc["pay_address"] = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	btc["pay_amount"] = "0.00123456"
	out := captureStdout(t, func() {
		if err := runTopup(&fakeDirectAPI{topupResp: btc}, topupRequest{amount: "10"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "exact amount") {
		t.Fatalf("btc caption missing:\n%s", out)
	}
	payload, err := decodeHalfBlockQR(qrBlock(out))
	if err != nil {
		t.Fatal(err)
	}
	want := "bitcoin:bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4?amount=0.00123456"
	if payload != want {
		t.Fatalf("btc QR decoded %q, want %q", payload, want)
	}

	xrp := directTopupResponse()
	xrp["pay_currency"] = "xrp"
	xrp["network"] = "xrp"
	xrp["pay_address"] = "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh"
	xrp["pay_amount"] = "12.5"
	xrp["payin_extra_id"] = "24680"
	out = captureStdout(t, func() {
		if err := runTopup(&fakeDirectAPI{topupResp: xrp}, topupRequest{amount: "10"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Memo / destination tag: 24680") {
		t.Fatalf("memo not in text output:\n%s", out)
	}
	if !strings.Contains(out, "This QR does not include the memo/destination tag. Enter this tag as well: 24680") {
		t.Fatalf("memo warning missing:\n%s", out)
	}
	if !strings.Contains(out, "address only — enter the amount shown above") {
		t.Fatalf("xrp should be address-only:\n%s", out)
	}
	payload, err = decodeHalfBlockQR(qrBlock(out))
	if err != nil {
		t.Fatal(err)
	}
	if payload != "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh" {
		t.Fatalf("xrp QR decoded %q", payload)
	}
}

func TestPayAmountStringNoFloatArtifacts(t *testing.T) {
	// encoding/json stores JSON numbers as float64.
	if got := payAmountString(map[string]any{"pay_amount": 0.09851181}); got != "0.09851181" {
		t.Fatalf("ltc-like float = %q", got)
	}
	if got := payAmountString(map[string]any{"pay_amount": 6.768e-05}); got != "0.00006768" {
		t.Fatalf("btc-like float = %q", got)
	}
	if got := payAmountString(map[string]any{"pay_amount": 0.01226249}); got != "0.01226249" {
		t.Fatalf("xmr-like float = %q", got)
	}
	if got := payAmountString(map[string]any{"pay_amount": "0.014000"}); got != "0.014000" {
		t.Fatalf("string passthrough = %q", got)
	}
}

func TestEIP681VerifiedTokens(t *testing.T) {
	const evm = "0x52908400098527886E0F7030069857D2E4169EE7"
	cases := map[string]string{
		"usdcop":     "ethereum:0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85@10/transfer?address=" + evm + "&uint256=2500000",
		"usdcmatic":  "ethereum:0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359@137/transfer?address=" + evm + "&uint256=2500000",
		"usdcarc20":  "ethereum:0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E@43114/transfer?address=" + evm + "&uint256=2500000",
		"usdtarc20":  "ethereum:0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7@43114/transfer?address=" + evm + "&uint256=2500000",
		"usdtcelo":   "ethereum:0x48065fbBE25f71C9282ddf5e1cD6D6A887483D5e@42220/transfer?address=" + evm + "&uint256=2500000",
		"ethbase":    evm,
		"usdtarb":    evm,
		"maticusdce": evm,
	}
	for code, want := range cases {
		got, err := depositQRPayloadFor(depositQR{Address: evm, Amount: "2.5", Currency: code})
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if got != want {
			t.Fatalf("%s payload = %s\nwant %s", code, got, want)
		}
	}
}
