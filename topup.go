package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Polling cadence for the default path. Tests shorten these; production waits
// two seconds, backs off to thirty, and gives up after twenty minutes.
var (
	topupPollInitialInterval = 2 * time.Second
	topupPollMaxInterval     = 30 * time.Second
	topupPollTimeout         = 20 * time.Minute
)

// cmdTopup adds funds to the billing account.
//
//	tessera topup <amount>                  # default: pay by address
//	tessera topup <amount> --coin <code>    # another coin
//	tessera topup <amount> --invoice        # hosted invoice link only
//	tessera topup <amount> --wallet         # x402 with the local wallet
//	tessera topup <amount> --json           # machine-readable
//	tessera topup <amount> --wait|--no-wait # control polling
//	tessera topup --coins                   # list accepted coins
//
// The default path asks for a payment address (USDC on Base) and, on an
// interactive terminal, polls until the intent reaches a terminal state. The
// legacy --x402/--payment flags keep their old meaning: request a challenge or
// submit an externally signed authorization. --wallet signs locally with the
// opt-in wallet key; the key never leaves the machine.
func cmdTopup(args []string) {
	req, err := parseTopupArgs(args)
	if err != nil {
		fatal("%v", err)
	}
	if req.coins {
		if err := runTopupCoins(req); err != nil {
			fatal("%s", describeTopupError(err))
		}
		return
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
	invoice     bool
	wallet      bool
	json        bool
	wait        bool
	noWait      bool
	coins       bool
}

// parseTopupArgs resolves the positional amount and the value flags. The
// amount is validated for presence and a positive numeric shape only: the
// server owns the accepted range and the per-asset minimum. --payment implies
// the x402 rail, so the printed re-run instruction works without restating
// --x402. --coins is a standalone listing and needs no amount.
func parseTopupArgs(args []string) (topupRequest, error) {
	if hasFlag(args, "--coins") {
		return topupRequest{coins: true, json: hasFlag(args, "--json")}, nil
	}
	pos := positional(args)
	if len(pos) != 1 {
		return topupRequest{}, errors.New("usage: tessera topup <amount> [--coin <code>] [--invoice] [--wallet] [--json] [--wait|--no-wait]")
	}
	req := topupRequest{amount: pos[0]}
	if err := validateTopupAmount(req.amount); err != nil {
		return topupRequest{}, err
	}
	payCurrency, err := topupValueFlag(args, "--pay-currency")
	if err != nil {
		return topupRequest{}, err
	}
	coin, err := topupValueFlag(args, "--coin")
	if err != nil {
		return topupRequest{}, err
	}
	if coin != "" {
		if payCurrency != "" && payCurrency != coin {
			return topupRequest{}, errors.New("--coin and --pay-currency name different assets; use one")
		}
		payCurrency = coin
	}
	payment, err := topupValueFlag(args, "--payment")
	if err != nil {
		return topupRequest{}, err
	}
	req.payCurrency = payCurrency
	req.payment = payment
	req.x402 = hasFlag(args, "--x402") || payment != ""
	req.invoice = hasFlag(args, "--invoice")
	req.wallet = hasFlag(args, "--wallet")
	req.json = hasFlag(args, "--json")
	req.wait = hasFlag(args, "--wait")
	req.noWait = hasFlag(args, "--no-wait")
	switch {
	case req.wallet && req.x402:
		return topupRequest{}, errors.New("--wallet and --x402/--payment are different signers; use one")
	case req.wallet && req.invoice:
		return topupRequest{}, errors.New("--wallet and --invoice are different payment paths; use one")
	case req.invoice && req.x402:
		return topupRequest{}, errors.New("--invoice and --x402/--payment are different payment paths; use one")
	case req.wait && req.noWait:
		return topupRequest{}, errors.New("--wait and --no-wait conflict")
	case req.x402 && req.payCurrency != "":
		return topupRequest{}, errors.New("x402 always pays USDC on Base; --coin applies to the default address path")
	}
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
		return "", fmt.Errorf("%s requires a value — usage: tessera topup <amount> [--coin <code>] [--invoice] [--wallet] [--json] [--wait|--no-wait]", name)
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
// the step completed — an address created, an invoice created, a challenge
// issued, a settlement confirmed — and an error otherwise; describeTopupError
// turns that error into the printed guidance.
func runTopup(api billingAPI, req topupRequest) error {
	switch {
	case req.wallet:
		return runWalletTopup(api, req)
	case req.x402:
		return runTopupX402(api, req)
	default:
		return runTopupAddress(api, req)
	}
}

// runTopupX402 is the original externally-signed x402 path. It is kept
// unchanged for back-compatibility: without --payment it prints the challenge
// an external signer needs; with --payment it submits the signed payload.
func runTopupX402(api billingAPI, req topupRequest) error {
	resp, err := api.Topup(req.amount, "x402", req.payCurrency, req.payment)
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

// runTopupAddress is the default path: create a NOWPayments intent that
// returns a direct payment address (USDC on Base unless --coin says
// otherwise). A server that only returns an invoice URL falls back to the
// previous invoice output, so older deployments keep working.
func runTopupAddress(api billingAPI, req topupRequest) error {
	mode := ""
	if req.invoice {
		mode = "invoice"
	}
	var resp map[string]any
	var err error
	if m, ok := api.(topupModeAPI); ok {
		resp, err = m.TopupWithMode(req.amount, "nowpayments", req.payCurrency, "", mode)
	} else {
		resp, err = api.Topup(req.amount, "nowpayments", req.payCurrency, "")
	}
	if err != nil {
		return err
	}
	out := withAmount(resp, req.amount)
	if topupField(resp, "pay_address") != "—" {
		wait := shouldWait(req)
		if req.json {
			// With --wait the final object is the merged terminal state; without
			// it the create object is the single JSON document.
			if !wait {
				emitJSON(out)
			}
		} else {
			printTopupDirect(out)
		}
		if wait {
			return pollTopup(api, out, req)
		}
		return nil
	}
	if req.json {
		emitJSON(out)
		return nil
	}
	printTopupInvoice(resp)
	return nil
}

// runWalletTopup signs an x402 authorization with the local wallet and submits
// it. The wallet key never leaves the machine; only the signature is sent.
func runWalletTopup(api billingAPI, req topupRequest) error {
	acct, err := api.Account()
	if err != nil {
		return err
	}
	if !topupRailsOpen(acct, "x402") {
		return errors.New("x402 is closed on this account; use 'tessera topup <amount>' for the default address top-up")
	}
	wallet, err := loadWallet()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no local wallet; run 'tessera wallet init' first")
		}
		return err
	}
	key, err := walletPrivateKey(wallet)
	if err != nil {
		return err
	}
	challengeAPI, ok := api.(x402ChallengeAPI)
	if !ok {
		return errors.New("this server cannot issue x402 challenges")
	}
	challenge, err := challengeAPI.X402Challenge(req.amount)
	if err != nil {
		return err
	}
	if len(challenge) == 0 {
		return errors.New("x402 challenge response was empty")
	}
	header, err := signX402Payment(challenge, key, time.Now())
	if err != nil {
		return fmt.Errorf("sign x402 payment: %w", err)
	}
	var resp map[string]any
	if m, ok := api.(topupModeAPI); ok {
		resp, err = m.TopupWithMode(req.amount, "x402", "", header, "")
	} else {
		resp, err = api.Topup(req.amount, "x402", "", header)
	}
	if err != nil {
		return err
	}
	if req.json {
		emitJSON(resp)
		return nil
	}
	printX402Settled(resp)
	return nil
}

// runTopupCoins lists the coins the server accepts. It needs no credential:
// the currency list is public.
func runTopupCoins(req topupRequest) error {
	cfg, err := loadConfig()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config: %w", err)
	}
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.IndexerURL == "" {
		cfg.IndexerURL = defaultIndexer
	}
	if cfg.BillingAPIURL == "" {
		cfg.BillingAPIURL = defaultBillingURL
	}
	api := newBillingClient(cfg)
	lister, ok := api.(currencyAPI)
	if !ok {
		return errors.New("this server does not list top-up coins")
	}
	coins, err := lister.Currencies()
	if err != nil {
		return err
	}
	if req.json {
		emitJSON(coins)
		return nil
	}
	if len(coins) == 0 {
		fmt.Println("No coins are available for top-ups right now.")
		return nil
	}
	fmt.Println("Available top-up coins:")
	fmt.Println()
	for _, coin := range coins {
		marker := " "
		if isDefault, _ := coin["default"].(bool); isDefault {
			marker = "*"
		}
		fmt.Printf("  %s %-10s %s\n", marker, topupField(coin, "code"), topupField(coin, "name"))
	}
	fmt.Println()
	fmt.Println("Use --coin <code> to pay in a coin other than the default.")
	return nil
}

// shouldWait reports whether the default path should poll. Interactive text
// sessions wait by default; scripts and --json do not unless --wait is given.
func shouldWait(req topupRequest) bool {
	if req.invoice || req.noWait {
		return false
	}
	if req.wait {
		return true
	}
	if req.json {
		return false
	}
	return stdinIsTerminal()
}

// withAmount returns a copy of a response with amount_usd filled in when the
// server's create response omitted it. The copy keeps JSON output additive.
func withAmount(resp map[string]any, amount string) map[string]any {
	out := make(map[string]any, len(resp)+1)
	for k, v := range resp {
		out[k] = v
	}
	if _, ok := out["amount_usd"]; !ok {
		out["amount_usd"] = amount
	}
	return out
}

// topupRailsOpen reports whether the account's topup_rails map marks rail open.
func topupRailsOpen(acct map[string]any, rail string) bool {
	rails, ok := acct["topup_rails"].(map[string]any)
	if !ok {
		return false
	}
	open, ok := rails[rail].(bool)
	return ok && open
}

// pollTopup follows one intent until it reaches a terminal state. It uses
// exponential backoff and a bounded overall wait so a forgotten command does
// not spin forever.
func pollTopup(api billingAPI, initial map[string]any, req topupRequest) error {
	statusAPI, ok := api.(topupStatusAPI)
	if !ok {
		fmt.Fprintln(os.Stderr, "This server does not support top-up status polling; open the invoice URL or run 'tessera account' later.")
		return nil
	}
	id := topupField(initial, "payment_intent_id")
	if id == "—" {
		return errors.New("top-up response is missing payment_intent_id")
	}
	interval := topupPollInitialInterval
	deadline := time.Now().Add(topupPollTimeout)
	if !req.json {
		fmt.Println()
		fmt.Println("Waiting for payment...")
	}
	for {
		status, err := statusAPI.TopupStatus(id)
		if err != nil {
			return fmt.Errorf("poll top-up %s: %w", id, err)
		}
		switch topupField(status, "status") {
		case "completed":
			return printTopupCompleted(api, initial, status, req)
		case "expired", "failed":
			state := topupField(status, "status")
			if req.json {
				emitJSON(mergeTopup(initial, status))
			} else {
				fmt.Printf("Top-up %s.\n", state)
			}
			return fmt.Errorf("top-up %s", state)
		}
		if time.Now().After(deadline) {
			if req.json {
				emitJSON(mergeTopup(initial, status))
			} else {
				fmt.Println("Still pending; the payment has not confirmed yet.")
				fmt.Printf("Check later with 'tessera account' or open the invoice URL.\n")
			}
			return nil
		}
		time.Sleep(interval)
		interval *= 2
		if interval > topupPollMaxInterval {
			interval = topupPollMaxInterval
		}
	}
}

// printTopupCompleted renders a completed intent and, when available, the
// account's new balance. JSON mode emits the merged intent plus balance_usd.
func printTopupCompleted(api billingAPI, initial, status map[string]any, req topupRequest) error {
	out := mergeTopup(initial, status)
	balance := ""
	if acct, err := api.Account(); err == nil {
		balance = topupField(acct, "balance_usd")
		if balance != "—" {
			out["balance_usd"] = balance
		}
	}
	if req.json {
		emitJSON(out)
		return nil
	}
	credited := topupField(status, "credited_usd")
	if credited != "—" {
		fmt.Printf("Credited $%s", credited)
	} else {
		fmt.Print("Top-up completed")
	}
	if balance != "" && balance != "—" {
		fmt.Printf(", balance $%s", balance)
	}
	fmt.Println()
	return nil
}

// mergeTopup overlays status onto the create response without mutating either.
func mergeTopup(initial, status map[string]any) map[string]any {
	out := make(map[string]any, len(initial)+len(status))
	for k, v := range initial {
		out[k] = v
	}
	for k, v := range status {
		out[k] = v
	}
	return out
}

// printTopupDirect renders the default direct-address response. The amount to
// send is pay_amount (not amount_usd); the invoice URL remains a browser
// fallback.
func printTopupDirect(resp map[string]any) {
	fmt.Println("Top-up address created.")
	fmt.Println()
	fmt.Printf("  Payment intent:  %s\n", topupField(resp, "payment_intent_id"))
	fmt.Printf("  Amount:          $%s\n", topupField(resp, "amount_usd"))
	fmt.Printf("  Send:            %s %s on %s\n", topupField(resp, "pay_amount"), topupField(resp, "pay_currency"), topupField(resp, "network"))
	fmt.Printf("  Address:         %s\n", topupField(resp, "pay_address"))
	fmt.Printf("  Address expires: %s\n", topupField(resp, "address_expires_at"))
	fmt.Printf("  Invoice URL:     %s\n", topupField(resp, "invoice_url"))
	fmt.Println()
	fmt.Println("Send exactly the 'Send' amount to the address. Or open the invoice URL")
	fmt.Println("to pay in a browser. The balance is credited after the payment confirms.")
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
	nonce, err := topupChallengeNonce(accept)
	if err != nil {
		return fmt.Errorf("x402 challenge is missing the payment nonce: %w (%v)", err, be)
	}
	fmt.Println("x402 payment required — sign an EIP-3009 TransferWithAuthorization.")
	fmt.Println()
	fmt.Printf("  Network:          %s\n", topupField(accept, "network"))
	fmt.Printf("  Asset:            %s\n", topupField(accept, "asset"))
	fmt.Printf("  Pay to:           %s\n", topupField(accept, "payTo"))
	fmt.Printf("  Amount (atomic):  %s\n", topupField(accept, "amount"))
	fmt.Printf("  Challenge nonce:  %s\n", nonce)
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
// accepts[0].extra.x402ChallengeNonce. A missing or empty nonce is an error:
// without it there is nothing the external signer can bind, and printing a
// half-valid challenge invites signing a document the server will reject.
func topupChallengeNonce(accept map[string]any) (string, error) {
	extra, _ := accept["extra"].(map[string]any)
	nonce, _ := extra["x402ChallengeNonce"].(string)
	if nonce == "" {
		return "", errors.New("accepts[0].extra.x402ChallengeNonce is missing or empty")
	}
	return nonce, nil
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
		return describeBillingError(err)
	}
	return err.Error()
}
