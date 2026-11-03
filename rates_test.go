package main

import (
	"strings"
	"testing"
)

// The live card as GET /api/v1/billing/rates returned it on 2026-11-03.
func sampleRates() map[string]any {
	return map[string]any{
		"block_bytes": float64(41943040), "month_days": float64(30),
		"storage":   map[string]any{"usd_per_block_month": "0.0001048576", "usd_per_gb_month": "0.0025", "usd_per_tb_month": "2.5"},
		"upload":    map[string]any{"usd_per_block": "0.00004194304", "usd_per_gb": "0.001", "usd_per_tb": "1"},
		"download":  map[string]any{"usd_per_block": "0.0004194304", "usd_per_gb": "0.01", "usd_per_tb": "10"},
		"one_block": map[string]any{"total_usd": "0.00056623104"},
	}
}

func TestPrintRates(t *testing.T) {
	out, _ := captureOutput(t, func() { printRates(sampleRates()) })
	for _, want := range []string{
		"$2.50 per TB-month", "$0.0025 per GB-month", "$0.000105 per 40 MiB block per 30 days",
		"Uploads:", "$1.00 per TB", "$0.001 per GB", "$0.000042 per 40 MiB block",
		"Downloads:", "$10.00 per TB", "$0.01 per GB", "$0.000419 per 40 MiB block",
		"$0.000566", "TB = 10^12 bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rates output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"$8", "$5", "$20", "ingress free", "free ingress"} {
		if strings.Contains(out, bad) {
			t.Errorf("rates output contains stale %q:\n%s", bad, out)
		}
	}
}

func TestPrintRatesMissingFields(t *testing.T) {
	out, _ := captureOutput(t, func() { printRates(map[string]any{}) })
	if !strings.Contains(out, "—") {
		t.Fatalf("missing fields should render as a dash:\n%s", out)
	}
}
