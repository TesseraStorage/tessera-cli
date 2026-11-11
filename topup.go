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
//	tessera topup <amount>                  # default: NOWPayments deposit address
//	tessera topup <amount> --coin <code>    # another coin
//	tessera topup <amount> --invoice        # hosted invoice link only
//	tessera topup <amount> --json           # machine-readable
//	tessera topup <amount> --wait|--no-wait # control polling
//	tessera topup --coins                   # list accepted coins
//
// The default path asks for a NOWPayments deposit address (USDC on Base) and,
// on an interactive terminal, polls until the intent reaches a terminal state.
// --invoice asks for a hosted invoice page instead.
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
	invoice     bool
	json        bool
	wait        bool
	noWait      bool
	coins       bool
	minimums    bool
	quiet       bool
	qr          qrStyle
}

// parseTopupArgs resolves the positional amount and the value flags. The
// amount is validated for presence and a positive numeric shape only: the
// server owns the accepted range and the per-asset minimum. --coins is a
// standalone listing and needs no amount.
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
	if hasFlag(args, "--wallet") {
		return topupRequest{}, errors.New("--wallet is not a tessera topup flag; create a deposit address with: tessera topup <amount>")
	}
	if hasFlag(args, "--x402") || hasFlag(args, "--payment") {
		return topupRequest{}, errors.New("--x402 and --payment are not tessera topup flags; create a deposit address with: tessera topup <amount>")
	}
	if len(pos) != 1 {
		return topupRequest{}, errors.New("usage: tessera topup <amount> [--asset <code>] [--invoice] [--json] [--wait|--no-wait]")
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
	req.payCurrency = payCurrency
	req.invoice = hasFlag(args, "--invoice")
	req.json = hasFlag(args, "--json")
	req.wait = hasFlag(args, "--wait")
	req.noWait = hasFlag(args, "--no-wait")
	req.quiet = hasFlag(args, "--quiet") || hasFlag(args, "-q")
	style, err := parseQRStyle(args)
	if err != nil {
		return topupRequest{}, err
	}
	req.qr = style
	if req.wait && req.noWait {
		return topupRequest{}, errors.New("--wait and --no-wait conflict")
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
		return "", fmt.Errorf("%s requires a value — usage: tessera topup <amount> [--asset <code>] [--invoice] [--json] [--wait|--no-wait]", name)
	}
	return v, nil
}

// Top-up range in USD, the same on every CLI method (deposit address,
// browser invoice). The CLI enforces this floor even if the server floor
// is lower. moneyd also enforces its own range (422 amount_out_of_range).
const (
	topupMinUSD = 5
	topupMaxUSD = 10000
)

// topupRangeText is the one wording of the range used in help and errors.
const topupRangeText = "between $5 and $10,000"

// validateTopupAmount checks the amount locally before any request: a number
// between $5 and $10,000 after rounding down to whole cents (as the server
// does, so 4.999 is $4.99 and refused).
func validateTopupAmount(amount string) error {
	if strings.TrimSpace(amount) == "" {
		return errors.New("topup amount is required (USD) — usage: tessera topup <amount>  (" + topupRangeText + ")")
	}
	v, err := strconv.ParseFloat(amount, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return fmt.Errorf("topup amount %q is not a number (USD); top-ups are %s", amount, topupRangeText)
	}
	cents := math.Floor(math.Round(v*1e6) / 1e4)
	if cents < topupMinUSD*100 || cents > topupMaxUSD*100 {
		return fmt.Errorf("topup amount $%s is out of range: top-ups are %s", strconv.FormatFloat(cents/100, 'f', 2, 64), topupRangeText)
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
// the step completed — an address created or an invoice created — and an
// error otherwise; describeTopupError turns that error into the printed
// guidance.
func runTopup(api billingAPI, req topupRequest) error {
	return runTopupAddress(api, req)
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
			printTopupDirect(out, req)
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
func printTopupDirect(resp map[string]any, req topupRequest) {
	fmt.Println("Top-up address created.")
	fmt.Println()
	fmt.Printf("  Payment intent:  %s\n", topupField(resp, "payment_intent_id"))
	fmt.Printf("  Amount:          $%s\n", topupField(resp, "amount_usd"))
	sendAmt := payAmountString(resp)
	if sendAmt == "" {
		sendAmt = "—"
	}
	fmt.Printf("  Send:            %s %s\n", sendAmt, assetLabel(topupField(resp, "pay_currency"), topupField(resp, "network")))
	fmt.Printf("  Address:         %s\n", topupField(resp, "pay_address"))
	if extra := extraIDFromResponse(resp); extra != "" {
		fmt.Printf("  Memo / destination tag: %s\n", extra)
	}
	fmt.Printf("  Send before:     %s\n", localTimeLabel(topupField(resp, "address_expires_at"), time.Now()))
	fmt.Printf("  Invoice URL:     %s\n", topupField(resp, "invoice_url"))
	fmt.Printf("  Invoice open until: %s\n", localTimeLabel(topupField(resp, "expires_at"), time.Now()))
	fmt.Println()
	fmt.Println("Send exactly the 'Send' amount to the address before the 'Send before' time")
	fmt.Println("(the processor's rate quote for this address, usually ~20 minutes). After")
	fmt.Println("that, run the command again for a fresh quote, or open the invoice URL,")
	fmt.Println("which stays open longer and lets you pick any accepted coin in a browser.")
	fmt.Println("The balance is credited after the payment confirms.")
	printDepositQR(os.Stdout, depositQR{
		Address:  topupField(resp, "pay_address"),
		Currency: topupField(resp, "pay_currency"),
		Network:  topupField(resp, "network"),
		Amount:   payAmountString(resp),
		Expires:  topupField(resp, "address_expires_at"),
		Style:    req.qr,
		JSON:     req.json,
		Quiet:    req.quiet,
		Invoice:  req.invoice,
		ExtraID:  extraIDFromResponse(resp),
	})
}

// payAmountString reads the crypto amount to send. A JSON string is kept
// exactly so the 6-decimal conversion does not pick up a float artifact.
func payAmountString(m map[string]any) string {
	v, ok := m["pay_amount"]
	if !ok || v == nil {
		return ""
	}
	switch n := v.(type) {
	case string:
		return n
	case float64:
		// Shortest round-trip decimal (encoding/json stores numbers as float64).
		// A fixed 18-digit format would print binary leftovers such as
		// 0.098511810000000005.
		return strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return fmt.Sprint(n)
	}
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
// unexpected passes through as the client rendered it. Stored credentials
// are never part of err.
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
		return "amount_out_of_range — top-ups are " + topupRangeText
	case be.Status == http.StatusUnprocessableEntity && be.Code == "unknown_rail":
		return "unknown_rail — the server does not support that payment rail"
	case be.Status == http.StatusServiceUnavailable && be.Code == "payments_unconfigured":
		return "payments_unconfigured — payments are not configured on the server yet; try again later"
	case be.Status == http.StatusBadGateway && be.Code == "settlement_pending":
		return fmt.Sprintf("settlement_pending — transaction %s has not confirmed yet and may still land; retry shortly or run 'tessera account'", topupField(be.Extra, "transaction"))
	case be.Status == http.StatusBadGateway && be.Code == "processor_error":
		return "processor_error — the payment processor failed and nothing was charged; try again shortly"
	case be.Status == http.StatusPaymentRequired:
		return fmt.Sprintf("payment was refused: %s", dashIfEmpty(be.Code))
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

// checkTopupMinimum refuses BEFORE a deposit address is created when the
// amount is below a coin's processor minimum (a few coins sit above $5). A
// browser invoice is not pre-checked: the $5–$10,000 range applies and the
// payer picks the coin on the page. Any failure to read the minimum is
// ignored: the server enforces the same rule.
func checkTopupMinimum(api billingAPI, req topupRequest) error {
	if req.invoice {
		return nil
	}
	m, ok := api.(topupMinimumsAPI)
	if !ok {
		return nil
	}
	coin := req.payCurrency
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
	npMin := fmt.Sprint(np["minimum_usd"])
	deposit := fmt.Sprintf("$%.2f", float64(topupMinUSD))
	if parsed, err := strconv.ParseFloat(npMin, 64); err == nil && parsed > topupMinUSD {
		deposit = "$" + npMin
	} else if np["minimum_usd"] == nil {
		deposit = "unavailable right now"
	}
	fmt.Printf("Top-ups: %s on every method.\n", topupRangeText)
	hint := "(default; other coins: --asset <code>)"
	if req.payCurrency != "" {
		hint = "(--asset " + req.payCurrency + ")"
	}
	fmt.Printf("  1. Deposit address in %s %s: %s\n", assetLabel(fmt.Sprint(np["pay_currency"]), ""), hint, deposit)
	fmt.Printf("  2. Browser invoice (--invoice, pay in any accepted coin): $%.2f\n", float64(topupMinUSD))
	fmt.Printf("A few coins have a higher processor minimum for a deposit address: tessera topup --minimums --asset <code>\n")
	return nil
}

// smallTopupGuidance is what to do when a coin's processor minimum is above
// the amount: send more, or pay another way (every method starts at $5).
const smallTopupGuidance = `Enter at least that amount, or pay another way (top-ups are between $5 and $10,000):
  - another coin:     tessera topup <usd> --asset <code>   (list: tessera topup --coins)
  - browser invoice:  tessera topup <usd> --invoice`

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

Add funds through NOWPayments. Top-ups are between $5 and $10,000.

  1. Deposit address in a coin you choose (default: USDC on Base).
       tessera topup 5                     # USDC on Base
       tessera topup 10 --asset xmr        # Monero; also btc, eth, sol, ltc,
       tessera topup 15 --asset usdttrc20  # usdtbsc, usdcsol, ... (many coins)
     A few coins have a processor minimum above $5 for a deposit address;
     the command prints it instead of creating the address.
     Pay the quoted amount to the address from any wallet or exchange that
     supports that asset.

  2. Browser invoice: open a link and pay in any accepted coin
     (USDC, USDT, BTC, ETH, XMR, SOL, LTC and hundreds more).
       tessera topup 5 --invoice

Options:
  --asset <code>   coin for a deposit address (alias: --coin, --pay-currency);
                   a bare ticker like 'usdt' lists its networks
  --invoice        hosted browser invoice instead of an address
  --json           one machine-readable JSON document
  --wait|--no-wait poll until paid (default on an interactive terminal)
  --no-qr          do not print a QR code
  --qr=address     QR of the plain deposit address (exchange apps). The default
                   QR is a payment URI that includes the exact amount when a
                   widely supported scheme exists (Bitcoin, Litecoin, Dogecoin,
                   Bitcoin Cash, Monero, Ether on Ethereum, and USDC/USDT on
                   verified EVM chains); otherwise it is the address only.

On a terminal at least 60 columns wide, a deposit address is also shown as a
QR code. The caption says whether the QR fills in the amount or only the
address (enter the amount shown above). If the processor returns a memo or
destination tag, it is printed with the address and again next to the QR —
the QR does not include it. The QR is omitted for --invoice, --json,
--quiet, and when output is not a terminal.

Discover:
  tessera topup --coins                   # every accepted coin
  tessera topup --minimums [--asset <c>]  # the range, and a coin's minimum

Amounts outside $5-$10,000, or below a coin's processor minimum for a deposit
address, are refused before anything is created.
Times are shown in your local time zone.
`
