package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// cmdPromo redeems a promo code for the stored billing account.
//
//	tessera promo <code>
//
// POST /billing/promo {code}. moneyd answers 200 with the redemption summary
// (kind, value, periods_remaining — null for kinds that carry no periods, such
// as credit grants), 404 promo_not_found, or 409 for every other PromoError.
// The response carries no secrets, and the stored credential is never printed.
func cmdPromo(args []string) {
	code, err := parsePromoArgs(args)
	if err != nil {
		fatal("%v", err)
	}
	api, err := promoClient()
	if err != nil {
		fatal("%v", err)
	}
	if err := runPromo(api, code); err != nil {
		fatal("%s", describePromoError(err))
	}
}

// parsePromoArgs resolves the single positional code. Codes are sent trimmed
// and otherwise untouched: moneyd owns normalization, caps and expiry.
func parsePromoArgs(args []string) (string, error) {
	pos := positional(args)
	if len(pos) != 1 {
		return "", errors.New("usage: tessera promo <code>")
	}
	code := strings.TrimSpace(pos[0])
	if code == "" {
		return "", errors.New("promo code is required — usage: tessera promo <code>")
	}
	return code, nil
}

// promoClient loads the config and builds the billing client. Like account, it
// fails before any network request when no config or no billing credential
// exists.
func promoClient() (billingAPI, error) {
	cfg, err := loadConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoBillingCredentials
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if credentialLabel(cfg) == "" {
		return nil, errNoBillingCredentials
	}
	return newBillingClient(cfg), nil
}

// runPromo performs the redemption and prints the outcome. It returns nil when
// the code was accepted and an error otherwise; describePromoError turns that
// error into the printed guidance. No output is produced on failure.
func runPromo(api billingAPI, code string) error {
	resp, err := api.Promo(code)
	if err != nil {
		return err
	}
	printPromoResult(resp)
	return nil
}

// printPromoResult renders the redemption summary. periods_remaining is a
// number for free_periods and fee promos and null for credit grants; a null or
// missing value is omitted rather than rendered as a Go artifact.
func printPromoResult(resp map[string]any) {
	fmt.Println("Promo code redeemed.")
	fmt.Println()
	fmt.Printf("  Kind:              %s\n", topupField(resp, "kind"))
	fmt.Printf("  Value:             %s\n", topupField(resp, "value"))
	if periods, ok := accountUint64(resp, "periods_remaining"); ok {
		fmt.Printf("  Periods remaining: %d\n", periods)
	}
}

// describePromoError maps a failed redemption to guidance. Every code moneyd's
// promos.redeem raises becomes the reason (404 for promo_not_found, 409 for the
// rest); anything unexpected passes through as the client rendered it. The
// submitted code and stored credentials are never part of err.
func describePromoError(err error) string {
	var be *billingError
	if !errors.As(err, &be) {
		return err.Error()
	}
	switch {
	case be.Status == http.StatusNotFound && be.Code == "promo_not_found":
		return "promo_not_found — no promo code matches that value"
	case be.Status == http.StatusConflict && be.Code == "promo_already_redeemed":
		return "promo_already_redeemed — this account already redeemed that code"
	case be.Status == http.StatusConflict && be.Code == "promo_exhausted":
		return "promo_exhausted — the code has reached its redemption cap"
	case be.Status == http.StatusConflict && be.Code == "promo_inactive":
		return "promo_inactive — the code is expired or has been disabled"
	case be.Status == http.StatusConflict && be.Code == "promo_new_accounts_only":
		return "promo_new_accounts_only — the code is only for accounts that have never been funded"
	case be.Status == http.StatusConflict && be.Code == "no_active_subscription":
		return "no_active_subscription — that fee promo needs an active subscription"
	case be.Status == http.StatusConflict && be.Code == "promo_conflict":
		return "promo_conflict — the subscription already has a promo attached"
	case be.Status == http.StatusConflict && be.Code == "promo_invalid":
		return "promo_invalid — that is not a valid promo code"
	case be.Status == http.StatusUnauthorized:
		return describeBillingError(err)
	}
	return err.Error()
}
