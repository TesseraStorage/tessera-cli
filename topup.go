package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// cmdTopup adds funds to the billing account.
//
//	tessera topup <amount> [--pay-currency <asset>]
//	tessera topup <amount> --x402
//	tessera topup <amount> --payment <base64 X-PAYMENT>
//
// Without --x402 the command opens a NOWPayments hosted invoice (201). With
// --x402 it requests moneyd's x402 challenge (402): the printed network, asset,
// payTo, atomic amount, challenge nonce and maxTimeoutSeconds window are
// everything an external EIP-3009 signer needs, and the signed payload comes
// back through --payment (which implies the x402 rail). The base64 X-PAYMENT
// value is sent to moneyd unchanged and is never printed, like every stored
// credential.
func cmdTopup(args []string) {
	req, err := parseTopupArgs(args)
	if err != nil {
		fatal("%v", err)
	}
	api, err := topupClient()
	if err != nil {
		fatal("%v", err)
	}
	if err := runTopup(api, req); err != nil {
		fatal("%s", describeTopupError(err))
	}
}

// topupRequest is one parsed topup invocation.
type topupRequest struct {
	amount      string
	payCurrency string
	payment     string
	x402        bool
}

// parseTopupArgs resolves the positional amount and the value flags. The
// amount is validated for presence and a positive numeric shape only: moneyd
// owns the real $5–$10000 range and the per-asset minimum. --payment implies
// the x402 rail, so the printed re-run instruction works without restating
// --x402.
func parseTopupArgs(args []string) (topupRequest, error) {
	pos := positional(args)
	if len(pos) != 1 {
		return topupRequest{}, errors.New("usage: tessera topup <amount> [--x402] [--pay-currency <asset>] [--payment <base64 X-PAYMENT>]")
	}
	req := topupRequest{amount: pos[0]}
	if err := validateTopupAmount(req.amount); err != nil {
		return topupRequest{}, err
	}
	payCurrency, err := topupValueFlag(args, "--pay-currency")
	if err != nil {
		return topupRequest{}, err
	}
	payment, err := topupValueFlag(args, "--payment")
	if err != nil {
		return topupRequest{}, err
	}
	req.payCurrency = payCurrency
	req.payment = payment
	req.x402 = hasFlag(args, "--x402") || payment != ""
	return req, nil
}

// topupValueFlag returns a present value flag's argument, erroring when the
// flag was given without one. "" is not a usable value for either flag.
func topupValueFlag(args []string, name string) (string, error) {
	if !hasFlag(args, name) {
		return "", nil
	}
	v, ok := flagValue(args, name)
	if !ok || v == "" {
		return "", fmt.Errorf("%s requires a value — usage: tessera topup <amount> [--x402] [--pay-currency <asset>] [--payment <base64 X-PAYMENT>]", name)
	}
	return v, nil
}

// validateTopupAmount checks presence and a positive numeric shape only; the
// server owns the accepted range, so no bound is hardcoded here.
func validateTopupAmount(amount string) error {
	if strings.TrimSpace(amount) == "" {
		return errors.New("topup amount is required (USD) — usage: tessera topup <amount>")
	}
	v, err := strconv.ParseFloat(amount, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return fmt.Errorf("topup amount %q is not a positive number (USD)", amount)
	}
	return nil
}

// topupClient loads the config and builds the billing client. Like account, it
// fails before any network request when no config or no billing credential
// exists.
func topupClient() (billingAPI, error) {
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

// runTopup performs the request and prints the outcome. It returns nil when
// the step completed — an invoice created, a challenge issued, a settlement
// confirmed — and an error otherwise; describeTopupError turns that error into
// the printed guidance.
func runTopup(api billingAPI, req topupRequest) error {
	rail := "nowpayments"
	if req.x402 {
		rail = "x402"
	}
	resp, err := api.Topup(req.amount, rail, req.payCurrency, req.payment)
	if err != nil {
		var be *billingError
		if errors.As(err, &be) && be.Status == http.StatusPaymentRequired && be.Code == "" {
			// A 402 without an error code is moneyd's x402 challenge
			// (PaymentRequired), not a refusal: print what to sign and stop
			// one step short of the externally signed settlement.
			return printX402Challenge(req, be)
		}
		return err
	}
	if req.payment != "" {
		printX402Settled(resp)
		return nil
	}
	printTopupInvoice(resp)
	return nil
}

// printTopupInvoice renders the 201 NOWPayments invoice fields.
func printTopupInvoice(resp map[string]any) {
	fmt.Println("Top-up invoice created (NOWPayments).")
	fmt.Println()
	fmt.Printf("  Payment intent: %s\n", topupField(resp, "payment_intent_id"))
	fmt.Printf("  Invoice URL:    %s\n", topupField(resp, "invoice_url"))
	fmt.Printf("  Expires:        %s\n", topupField(resp, "expires_at"))
	fmt.Println()
	fmt.Println("Open the invoice URL to pay with crypto; the balance is credited after")
	fmt.Println("the processor confirms the payment.")
}

// printX402Challenge renders moneyd's PaymentRequired document. Only the
// fields an EIP-3009 signer needs are shown: the challenge nonce comes from
// accepts[0].extra.x402ChallengeNonce, and validBefore is deliberately absent
// (it is not in the response).
func printX402Challenge(req topupRequest, be *billingError) error {
	accept, ok := firstX402Accept(be.Extra)
	if !ok {
		return fmt.Errorf("x402 challenge is missing accepts: %v", be)
	}
	fmt.Println("x402 payment required — sign an EIP-3009 TransferWithAuthorization.")
	fmt.Println()
	fmt.Printf("  Network:          %s\n", topupField(accept, "network"))
	fmt.Printf("  Asset:            %s\n", topupField(accept, "asset"))
	fmt.Printf("  Pay to:           %s\n", topupField(accept, "payTo"))
	fmt.Printf("  Amount (atomic):  %s\n", topupField(accept, "amount"))
	fmt.Printf("  Challenge nonce:  %s\n", topupChallengeNonce(accept))
	fmt.Printf("  Max timeout:      %s seconds\n", topupField(accept, "maxTimeoutSeconds"))
	fmt.Println()
	fmt.Println("Build and sign the authorization externally (your signing key never")
	fmt.Println("touches the CLI), base64-encode the x402 payment JSON, then re-run:")
	fmt.Println()
	fmt.Printf("  tessera topup %s --x402 --payment <base64 X-PAYMENT>\n", req.amount)
	fmt.Println()
	fmt.Println("The server verifies the signature, settles on-chain, and credits your")
	fmt.Println("balance.")
	return nil
}

// printX402Settled renders the 200 settlement result and notes the
// PAYMENT-RESPONSE receipt header; the header's contents are not printed.
func printX402Settled(resp map[string]any) {
	balance := topupField(resp, "balance_usd")
	if balance != "—" {
		balance = "$" + balance
	}
	fmt.Println("x402 payment settled.")
	fmt.Println()
	fmt.Printf("  Transaction: %s\n", topupField(resp, "transaction"))
	fmt.Printf("  Network:     %s\n", topupField(resp, "network"))
	fmt.Printf("  Balance:     %s\n", balance)
	fmt.Println()
	fmt.Println("The server returned a PAYMENT-RESPONSE header with the settlement")
	fmt.Println("receipt; run 'tessera account' to see the updated balance.")
}

// topupChallengeNonce reads the server-issued challenge nonce from
// accepts[0].extra.x402ChallengeNonce.
func topupChallengeNonce(accept map[string]any) string {
	extra, ok := accept["extra"].(map[string]any)
	if !ok {
		return "—"
	}
	return topupField(extra, "x402ChallengeNonce")
}

// firstX402Accept returns accepts[0] from a PaymentRequired document preserved
// in billingError.Extra.
func firstX402Accept(extra map[string]any) (map[string]any, bool) {
	accepts, ok := extra["accepts"].([]any)
	if !ok || len(accepts) == 0 {
		return nil, false
	}
	accept, ok := accepts[0].(map[string]any)
	return accept, ok
}

// topupField renders one decoded JSON field for display. Missing, null and
// empty values render as an em dash.
func topupField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return "—"
	}
	if s, ok := v.(string); ok && s == "" {
		return "—"
	}
	return fmt.Sprint(v)
}

// describeTopupError maps a failed topup to the guidance printed before the
// command exits non-zero. moneyd's codes become the action to take; anything
// unexpected passes through as the client rendered it. The X-PAYMENT contents
// and stored credentials are never part of err.
func describeTopupError(err error) string {
	var be *billingError
	if !errors.As(err, &be) {
		return err.Error()
	}
	switch {
	case be.Status == http.StatusConflict && be.Code == "below_asset_minimum":
		if minimum := topupField(be.Extra, "minimum_usd"); minimum != "—" {
			return fmt.Sprintf("below_asset_minimum — the minimum for this payment asset is $%s; retry with at least that amount", minimum)
		}
		return "below_asset_minimum — retry with a larger amount"
	case be.Status == http.StatusUnprocessableEntity && be.Code == "amount_out_of_range":
		return "amount_out_of_range — moneyd accepts top-ups from $5 to $10000"
	case be.Status == http.StatusUnprocessableEntity && be.Code == "unknown_rail":
		return "unknown_rail — moneyd does not support that payment rail"
	case be.Status == http.StatusServiceUnavailable && be.Code == "payments_unconfigured":
		return "payments_unconfigured — payments are not configured on the server yet; try again later"
	case be.Status == http.StatusBadGateway && be.Code == "settlement_pending":
		return fmt.Sprintf("settlement_pending — transaction %s has not confirmed yet and may still land; retry shortly or run 'tessera account'", topupField(be.Extra, "transaction"))
	case be.Status == http.StatusBadGateway && be.Code == "processor_error":
		return "processor_error — the payment processor failed and nothing was charged; try again shortly"
	case be.Status == http.StatusPaymentRequired:
		return fmt.Sprintf("x402 refused the payment: %s", dashIfEmpty(be.Code))
	case be.Status == http.StatusUnauthorized:
		return fmt.Sprintf("authentication failed — run 'tessera signup' or 'tessera login' to get billing credentials (%s)", be)
	}
	return err.Error()
}
