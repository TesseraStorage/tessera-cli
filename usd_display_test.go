package main

import "testing"

func TestDisplayUSD(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"0.00":          "0.00",
		"0.25":          "0.25",
		"12":            "12.00",
		"0.0499":        "0.0499",
		"0.04989375392": "0.049894",
		"0.00004194304": "0.000042",
		"0.0004194304":  "0.000419",
		"0.00056623104": "0.000566",
		"0.10000000001": "0.10",
		"1.23456":       "1.23",
		"2.5":           "2.50",
		"0.00000004":    "<0.000001",
		"0.00000":       "0.00",
		"-0.0000454":    "-0.000045",
	}
	for in, want := range cases {
		if got := displayUSD(in); got != want {
			t.Errorf("displayUSD(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrialSummaryRoundsBalanceButJSONKeepsExact(t *testing.T) {
	acct := map[string]any{
		"balance_usd": "0.24989375392",
		"readiness":   map[string]any{"state": "free_trial_active", "can_upload": true, "free_trial": true, "trial_credit_usd": "0.25", "trial_credit_posted": true},
	}
	v := buildTrialView(acct)
	if v.BalanceUSD != "0.24989375392" {
		t.Errorf("json balance = %q", v.BalanceUSD)
	}
	if want := "Free trial: $0.249894 of your one-time $0.25 credit left."; len(v.Summary) < len(want) || v.Summary[:len(want)] != want {
		t.Errorf("summary = %q", v.Summary)
	}
}
