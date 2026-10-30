package main

import "testing"

func TestDisplayUSD(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"0.00":          "0.00",
		"0.05":          "0.05",
		"12":            "12",
		"0.0499":        "0.0499",
		"0.04989375392": "0.0499",
		"0.05000000001": "0.05",
		"1.23456":       "1.2346",
		"0.00000004":    "<0.0001",
		"0.00000":       "0.00",
	}
	for in, want := range cases {
		if got := displayUSD(in); got != want {
			t.Errorf("displayUSD(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrialSummaryRoundsBalanceButJSONKeepsExact(t *testing.T) {
	acct := map[string]any{
		"balance_usd": "0.04989375392",
		"readiness":   map[string]any{"state": "free_trial_active", "can_upload": true, "free_trial": true, "trial_credit_usd": "0.05", "trial_credit_posted": true},
	}
	v := buildTrialView(acct)
	if v.BalanceUSD != "0.04989375392" {
		t.Errorf("json balance = %q", v.BalanceUSD)
	}
	if want := "Free trial: $0.0499 of your one-time $0.05 credit left."; len(v.Summary) < len(want) || v.Summary[:len(want)] != want {
		t.Errorf("summary = %q", v.Summary)
	}
}
