package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// tessera rates (v1.12.0, Money 10): the live price card from the public
// GET /billing/rates endpoint. Nothing is hard-coded here, so the CLI always
// shows what the billing system charges right now.

// ratesAPI reads GET /billing/rates (public, no login).
type ratesAPI interface {
	Rates() (map[string]any, error)
}

// Rates reads the public live rate card.
func (b *httpBilling) Rates() (map[string]any, error) {
	req, err := b.request(http.MethodGet, "/billing/rates", nil)
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authNone, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}

func runRates(args []string) error {
	cfg, err := loadConfig()
	if err != nil || cfg == nil {
		cfg = &Config{}
	}
	if cfg.IndexerURL == "" {
		cfg.IndexerURL = defaultIndexer
	}
	if cfg.BillingAPIURL == "" {
		cfg.BillingAPIURL = defaultBillingURL
	}
	r, ok := newBillingClient(cfg).(ratesAPI)
	if !ok {
		return errors.New("this build cannot read the rate card")
	}
	doc, err := r.Rates()
	if err != nil {
		return err
	}
	if hasFlag(args, "--json") {
		emitJSON(doc)
		return nil
	}
	printRates(doc)
	return nil
}

func ratesSection(doc map[string]any, key string) map[string]any {
	m, _ := doc[key].(map[string]any)
	return m
}

func rateStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// rateUSD prints a price with up to 6 decimals ($0.000042), or 2 from $1 up.
func rateUSD(s string) string {
	if s == "" {
		return "—"
	}
	return "$" + displayUSD(s)
}

func printRates(doc map[string]any) {
	st, up, dl, one := ratesSection(doc, "storage"), ratesSection(doc, "upload"), ratesSection(doc, "download"), ratesSection(doc, "one_block")
	fmt.Println("Tessera rates (live, from the billing system):")
	fmt.Printf("  %-10s %s per TB-month  (%s per GB-month, %s per 40 MiB block per 30 days)\n", "Storage:",
		rateUSD(rateStr(st, "usd_per_tb_month")), rateUSD(rateStr(st, "usd_per_gb_month")), rateUSD(rateStr(st, "usd_per_block_month")))
	fmt.Printf("  %-10s %s per TB  (%s per GB, %s per 40 MiB block)\n", "Uploads:",
		rateUSD(rateStr(up, "usd_per_tb")), rateUSD(rateStr(up, "usd_per_gb")), rateUSD(rateStr(up, "usd_per_block")))
	fmt.Printf("  %-10s %s per TB  (%s per GB, %s per 40 MiB block)\n", "Downloads:",
		rateUSD(rateStr(dl, "usd_per_tb")), rateUSD(rateStr(dl, "usd_per_gb")), rateUSD(rateStr(dl, "usd_per_block")))
	if one != nil {
		fmt.Printf("  One block (upload + 30 days stored + one full download): %s\n", rateUSD(rateStr(one, "total_usd")))
	}
	fmt.Println("  TB = 10^12 bytes. Billing is per 40 MiB block; storage is charged per day.")
	fmt.Println("  No subscription needed; top up any amount from $0.01: tessera topup 1")
}
