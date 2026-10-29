package main

import "testing"

// Regression: "--asset <code>" after the amount must not be read as a second
// positional argument (v1.11.0 printed the usage error).
func TestParseTopupAssetAfterAmount(t *testing.T) {
	for _, args := range [][]string{
		{"5", "--asset", "xmr", "--no-wait"},
		{"--asset", "xmr", "5"},
		{"5", "--asset", "XMR"},
	} {
		req, err := parseTopupArgs(args)
		if err != nil {
			t.Fatalf("%v: unexpected error %v", args, err)
		}
		if req.amount != "5" || req.payCurrency != "xmr" {
			t.Fatalf("%v: got amount=%q payCurrency=%q", args, req.amount, req.payCurrency)
		}
	}
	if _, err := parseTopupArgs([]string{"5", "--asset", "xmr", "--coin", "btc"}); err == nil {
		t.Fatal("conflicting --asset/--coin must error")
	}
}
