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
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Print(topupHelp)
		return
	}
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
	if req.minimums {
		if err := runTopupMinimums(req); err != nil {
			fatal("%s", describeTopupError(err))
		}
		return
	}
	api, err := topupClient()
	if err != nil {
		fatal("%v", err)
	}
	if err := runTopup(api, req); err != nil {
		if req.json {
			// One JSON document on stdout whether the request succeeded or
			// failed, so an agent never has to parse a human sentence.
			emitTopupErrorJSON(err)
			os.Exit(1)
		}
		fatal("%s", describeTopupError(err))
	}
}

// emitTopupErrorJSON renders a failed top-up as one machine-readable object.
// It carries the API's error code, HTTP status and the actionable fields
// (minimum_usd, transaction) when present.
func emitTopupErrorJSON(err error) {
	out := map[string]any{
		"error":   "client_error",
		"message": describeTopupError(err),
	}
	var be *billingError
	if errors.As(err, &be) {
		code := be.Code
		if code == "" {
			code = "http_error"
		}
		out["error"] = code
		out["status"] = be.Status
		if minimum := topupField(be.Extra, "minimum_usd"); minimum != "—" {
			out["minimum_usd"] = minimum
		}
		if tx := topupField(be.Extra, "transaction"); tx != "—" {
			out["transaction"] = tx
		}
	}
	emitJSON(out)
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
	minimums    bool
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
	if hasFlag(args, "--minimums") {
		coin, _ := flagValue(args, "--coin")
		if a, ok := flagValue(args, "--asset"); ok && a != "" {
			coin = a
		}
		coin = strings.ToLower(strings.TrimSpace(coin))
		return topupRequest{minimums: true, payCurrency: coin, json: hasFlag(args, "--json")}, nil
	}
	pos := positional(args)
	if len(pos) != 1 {
		return topupRequest{}, errors.New("usage: tessera topup <amount> [--asset <code>] [--invoice] [--wallet] [--json] [--wait|--no-wait]")
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
	asset, err := topupValueFlag(args, "--asset")
	if err != nil {
		return topupRequest{}, err
	}
	if asset != "" {
		if coin != "" && !strings.EqualFold(coin, asset) {
			return topupRequest{}, errors.New("--asset and --coin name different assets; use one")
		}
		coin = asset
	}
	coin = strings.ToLower(strings.TrimSpace(coin))
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
		return "", fmt.Errorf("%s requires a value — usage: tessera topup <amount> [--asset <code>] [--invoice] [--wallet] [--json] [--wait|--no-wait]", name)
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
	if !req.invoice && req.payCurrency != "" {
		code, err := resolveAssetCode(api, req.payCurrency)
		if err != nil {
			return err
		}
		req.payCurrency = code
	}
	if err := checkTopupMinimum(api, req); err != nil {
		return err
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
		fmt.Printf("  %s %-12s %s\n", marker, topupField(coin, "code"), topupField(coin, "name"))
	}
	fmt.Println()
	fmt.Println("* = default (USDC on Base, the lowest minimum).")
	fmt.Println("Get a deposit address in another coin: tessera topup <usd> --asset <code>")
	fmt.Println("Live minimum for a coin:              tessera topup --minimums --asset <code>")
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
	fmt.Printf("  Send:            %s %s\n", topupField(resp, "pay_amount"), assetLabel(topupField(resp, "pay_currency"), topupField(resp, "network")))
	fmt.Printf("  Address:         %s\n", topupField(resp, "pay_address"))
	fmt.Printf("  Send before:     %s\n", localTimeLabel(topupField(resp, "address_expires_at"), time.Now()))
	fmt.Printf("  Invoice URL:     %s\n", topupField(resp, "invoice_url"))
	fmt.Printf("  Invoice open until: %s\n", localTimeLabel(topupField(resp, "expires_at"), time.Now()))
	fmt.Println()
	fmt.Println("Send exactly the 'Send' amount to the address before the 'Send before' time")
	fmt.Println("(the processor's rate quote for this address, usually ~20 minutes). After")
	fmt.Println("that, run the command again for a fresh quote, or open the invoice URL,")
	fmt.Println("which stays open longer and lets you pick any accepted coin in a browser.")
	fmt.Println("The balance is credited after the payment confirms.")
}

// printTopupInvoice renders the 201 NOWPayments invoice fields.
func printTopupInvoice(resp map[string]any) {
	fmt.Println("Top-up invoice created (NOWPayments).")
	fmt.Println()
	fmt.Printf("  Payment intent: %s\n", topupField(resp, "payment_intent_id"))
	fmt.Printf("  Invoice URL:    %s\n", topupField(resp, "invoice_url"))
	fmt.Printf("  Open until:     %s\n", localTimeLabel(topupField(resp, "expires_at"), time.Now()))
	fmt.Println()
	fmt.Println("Open the invoice URL in a browser and pick any accepted coin (USDC, USDT,")
	fmt.Println("BTC, ETH, XMR, SOL, LTC and hundreds more; each coin shows its own minimum).")
	fmt.Println("The balance is credited after the processor confirms the payment.")
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
// command exits non-zero. The API's codes become the action to take; anything
// unexpected passes through as the client rendered it. The X-PAYMENT contents
// and stored credentials are never part of err.
func describeTopupError(err error) string {
	var be *billingError
	if !errors.As(err, &be) {
		return err.Error()
	}
	switch {
	case be.Status == http.StatusConflict && be.Code == "below_asset_minimum":
		asset := "this coin"
		if coin := topupField(be.Extra, "pay_currency"); coin != "—" {
			asset = assetLabel(coin, "")
		}
		how := "a deposit address"
		if topupField(be.Extra, "mode") == "invoice" {
			how = "a browser invoice"
		}
		head := "below_asset_minimum — this amount is below the payment processor's minimum, so nothing was created."
		if minimum := topupField(be.Extra, "minimum_usd"); minimum != "—" {
			head = fmt.Sprintf("below_asset_minimum — the minimum for %s in %s is $%s (set by the payment processor), so nothing was created.", how, asset, minimum)
		}
		return head + "\n" + smallTopupGuidance
	case be.Status == http.StatusUnprocessableEntity && be.Code == "unknown_currency":
		return "unknown_currency — that coin is not accepted; list coins with 'tessera topup --coins'"
	case be.Status == http.StatusUnprocessableEntity && be.Code == "amount_out_of_range":
		return "amount_out_of_range — the server rejected this amount as outside its accepted range"
	case be.Status == http.StatusUnprocessableEntity && be.Code == "unknown_rail":
		return "unknown_rail — the server does not support that payment rail"
	case be.Status == http.StatusServiceUnavailable && be.Code == "payments_unconfigured":
		return "payments_unconfigured — payments are not configured on the server yet; try again later"
	case be.Status == http.StatusBadGateway && be.Code == "settlement_pending":
		return fmt.Sprintf("settlement_pending — transaction %s has not confirmed yet and may still land; retry shortly or run 'tessera account'", topupField(be.Extra, "transaction"))
	case be.Status == http.StatusBadGateway && be.Code == "processor_error":
		return "processor_error — the payment processor failed and nothing was charged; try again shortly"
	case be.Status == http.StatusPaymentRequired:
		return fmt.Sprintf("x402 refused the payment: %s", dashIfEmpty(be.Code))
	case be.Status == http.StatusUnauthorized,
		be.Status == http.StatusForbidden && be.Code == "account_closed":
		return describeBillingError(err)
	}
	return err.Error()
}

// topupMinimumsAPI reads GET /billing/topup/minimums (public). Optional so a
// test fake without it simply skips the pre-check; the server validates too.
type topupMinimumsAPI interface {
	TopupMinimums(payCurrency string) (map[string]any, error)
}

// checkTopupMinimum warns and refuses BEFORE an invoice or address is
// created when the amount is below the processor's live minimum for the coin
// (an invoice is checked against the default, cheapest coin). Any failure to
// read the minimum is ignored: the server enforces the same rule.
func checkTopupMinimum(api billingAPI, req topupRequest) error {
	m, ok := api.(topupMinimumsAPI)
	if !ok {
		return nil
	}
	coin := req.payCurrency
	if req.invoice {
		coin = ""
	}
	doc, err := m.TopupMinimums(coin)
	if err != nil {
		return nil
	}
	rails, _ := doc["rails"].(map[string]any)
	np, _ := rails["nowpayments"].(map[string]any)
	minStr, _ := np["minimum_usd"].(string)
	if minStr == "" {
		return nil
	}
	minimum, err1 := strconv.ParseFloat(minStr, 64)
	amount, err2 := strconv.ParseFloat(req.amount, 64)
	if err1 != nil || err2 != nil || amount >= minimum-1e-9 {
		return nil
	}
	extra := map[string]any{"error": "below_asset_minimum", "minimum_usd": minStr}
	if c, _ := np["pay_currency"].(string); c != "" {
		extra["pay_currency"] = c
	}
	if req.invoice {
		extra["mode"] = "invoice"
	}
	return &billingError{Status: http.StatusConflict, Code: "below_asset_minimum", Extra: extra}
}

// runTopupMinimums prints the live per-rail minimums (public endpoint, no
// login needed).
func runTopupMinimums(req topupRequest) error {
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
	m, ok := newBillingClient(cfg).(topupMinimumsAPI)
	if !ok {
		return errors.New("this build cannot read top-up minimums")
	}
	doc, err := m.TopupMinimums(req.payCurrency)
	if err != nil {
		return err
	}
	if req.json {
		emitJSON(doc)
		return nil
	}
	rails, _ := doc["rails"].(map[string]any)
	np, _ := rails["nowpayments"].(map[string]any)
	x, _ := rails["x402"].(map[string]any)
	fmt.Printf("Top-up minimums (live):\n")
	npMin := fmt.Sprint(np["minimum_usd"])
	if np["minimum_usd"] == nil {
		npMin = "unavailable right now"
	} else {
		npMin = "$" + npMin
	}
	fmt.Printf("  1. x402 wallet, USDC on Base (--wallet, --x402, --payment): $%s — any amount from the floor\n", fmt.Sprint(x["minimum_usd"]))
	hint := "(default; other coins: --asset <code>)"
	if req.payCurrency != "" {
		hint = "(--asset " + req.payCurrency + ")"
	}
	fmt.Printf("  2. Deposit address in %s %s: %s\n", assetLabel(fmt.Sprint(np["pay_currency"]), ""), hint, npMin)
	invMin := "the cheapest coin's minimum"
	if req.payCurrency == "" {
		invMin = npMin
	}
	fmt.Printf("  3. Browser invoice (--invoice, pay in any accepted coin): from %s; each coin shows its own minimum\n", invMin)
	fmt.Printf("Tessera floor (all rails): $%s. Crypto minimums are set by the payment processor (NOWPayments) and move with fees.\n", fmt.Sprint(doc["floor_usd"]))
	fmt.Printf("Other coins: tessera topup --minimums --asset <code>  (list coins: tessera topup --coins)\n")
	return nil
}

// smallTopupGuidance is the one accurate answer to "how do I pay less than
// the processor minimum": only x402 has no processor minimum.
const smallTopupGuidance = `Amounts under the processor minimum (down to $0.01) work only with x402 (USDC on Base):
  - built-in wallet:  tessera wallet init   (fund it with USDC on Base)
                      tessera topup <usd> --wallet
  - your own signer:  tessera topup <usd> --x402    (prints what to sign)
                      tessera topup <usd> --payment <base64 X-PAYMENT>
Live minimums per coin: tessera topup --minimums [--asset <code>]`

// assetLabels names common coins in plain words. Unknown codes fall back to
// the ticker plus network.
var assetLabels = map[string]string{
	"usdcbase": "USDC on Base", "usdc": "USDC on Ethereum", "usdcmatic": "USDC on Polygon",
	"usdcsol": "USDC on Solana", "usdcbsc": "USDC on BNB Chain", "usdcarb": "USDC on Arbitrum",
	"usdcop": "USDC on Optimism", "usdcalgo": "USDC on Algorand",
	"usdterc20": "USDT on Ethereum", "usdttrc20": "USDT on Tron", "usdtbsc": "USDT on BNB Chain",
	"usdtmatic": "USDT on Polygon", "usdtsol": "USDT on Solana", "usdtarb": "USDT on Arbitrum",
	"usdtop": "USDT on Optimism", "usdtton": "USDT on TON",
	"btc": "Bitcoin (BTC)", "btclightning": "Bitcoin Lightning", "eth": "Ether (ETH) on Ethereum",
	"ethbase": "Ether (ETH) on Base", "etharb": "Ether (ETH) on Arbitrum", "xmr": "Monero (XMR)",
	"sol": "Solana (SOL)", "ltc": "Litecoin (LTC)", "trx": "TRON (TRX)", "doge": "Dogecoin (DOGE)",
	"bnbbsc": "BNB on BNB Chain", "ton": "Toncoin (TON)", "xrp": "XRP", "ada": "Cardano (ADA)",
}

var networkLabels = map[string]string{
	"base": "Base", "eth": "Ethereum", "bsc": "BNB Chain", "matic": "Polygon", "sol": "Solana",
	"trx": "Tron", "arbitrum": "Arbitrum", "op": "Optimism", "ton": "TON",
}

func assetLabel(code, network string) string {
	c := strings.ToLower(strings.TrimSpace(code))
	if c == "" || c == "—" {
		return "—"
	}
	if l, ok := assetLabels[c]; ok {
		return l
	}
	n := strings.ToLower(strings.TrimSpace(network))
	if n == "" || n == "—" || n == c {
		return strings.ToUpper(c)
	}
	if l, ok := networkLabels[n]; ok {
		n = l
	}
	return strings.ToUpper(c) + " on " + n
}

// localTimeLabel renders an RFC 3339 server time in the user's local zone
// with the zone named and a relative hint, so an expiry is never misread as
// already past because it was printed in UTC.
func localTimeLabel(raw string, now time.Time) string {
	if raw == "" || raw == "—" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	lt := t.In(time.Local)
	_, off := lt.Zone()
	zone := fmt.Sprintf("UTC%+03d:%02d", off/3600, abs(off%3600)/60)
	rel := ""
	d := t.Sub(now).Round(time.Minute)
	switch {
	case d > 0:
		rel = "in " + humanDuration(d)
	case d < 0:
		rel = "expired " + humanDuration(-d) + " ago"
	default:
		rel = "now"
	}
	return fmt.Sprintf("%s %s (%s)", lt.Format("2006-01-02 15:04"), zone, rel)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func humanDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dmin", h, m)
}

// resolveAssetCode turns a ticker such as "usdt" into a catalogue code, or
// explains the choices when a ticker exists on several networks. Exact codes
// pass through; an unreadable catalogue defers to the server.
func resolveAssetCode(api billingAPI, code string) (string, error) {
	if code == "" {
		return "", nil
	}
	lister, ok := api.(currencyAPI)
	if !ok {
		return code, nil
	}
	coins, err := lister.Currencies()
	if err != nil || len(coins) == 0 {
		return code, nil
	}
	var matches []string
	for _, c := range coins {
		cc := strings.ToLower(topupField(c, "code"))
		if cc == code {
			return code, nil
		}
		if strings.HasPrefix(cc, code) {
			matches = append(matches, fmt.Sprintf("%s (%s)", cc, topupField(c, "name")))
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%q is not an accepted coin; list coins with 'tessera topup --coins'", code)
	}
	if len(matches) > 12 {
		matches = append(matches[:12], "…")
	}
	return "", fmt.Errorf("%s is available on several networks; pick one with --asset <code>:\n  %s", strings.ToUpper(code), strings.Join(matches, "\n  "))
}

const topupHelp = `Usage: tessera topup <usd> [options]

Three ways to pay:

  1. Built-in x402 wallet: USDC on Base. Best for agents; any amount from $0.01.
       tessera wallet init                 # creates a local key (~/.tessera/wallet.json)
       tessera wallet fund                 # how to send USDC on Base to it
       tessera topup 0.50 --wallet
     Own signer instead: tessera topup <usd> --x402 (prints what to sign), then
       tessera topup <usd> --payment <base64 X-PAYMENT>

  2. Deposit address in a coin you choose (default: USDC on Base).
       tessera topup 5                     # USDC on Base
       tessera topup 5 --asset xmr         # Monero; also btc, eth, sol, ltc,
       tessera topup 5 --asset usdttrc20   # usdtbsc, usdcsol, ... (358 coins)
     Each coin has a minimum set by the payment processor (NOWPayments),
     fetched live: about $0.38 for USDC on Base, more for most other coins.

  3. Browser invoice: open a link and pay in any accepted coin
     (USDC, USDT, BTC, ETH, XMR, SOL, LTC and hundreds more).
       tessera topup 5 --invoice

Options:
  --asset <code>   coin for a deposit address (alias: --coin, --pay-currency);
                   a bare ticker like 'usdt' lists its networks
  --invoice        hosted browser invoice instead of an address
  --wallet         pay from the built-in x402 wallet
  --x402 / --payment <b64>   external x402 signer
  --json           one machine-readable JSON document
  --wait|--no-wait poll until paid (default on an interactive terminal)

Discover:
  tessera topup --coins                   # every accepted coin
  tessera topup --minimums [--asset <c>]  # live minimum per method / coin

Amounts below a coin's processor minimum are refused before anything is
created; under that minimum (down to $0.01) only x402 (method 1) works.
Times are shown in your local time zone.
`
