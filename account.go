package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmdAccount shows the billing account dashboard.
//
//	tessera account [--json]
//
// GET /account is authenticated with whichever billing credential is stored: a
// human session when one exists, otherwise an agent key. The human table names
// the active credential but never prints either secret. period_end and
// days_left are null for zero-burn accounts (moneyd dunning.py:171-175) and
// render as an em dash. --json emits the server object unchanged plus a
// non-secret credential tag, so scripts can tell which identity answered.
func cmdAccount(args []string) {
	if pos := positional(args); len(pos) > 0 && pos[0] == "delete" {
		cmdAccountDelete(args[1:])
		return
	}
	acct, credential, err := fetchAccountSummary()
	if err != nil {
		fatal("%s", describeBillingError(err))
	}
	if hasFlag(args, "--json") {
		emitJSON(accountWithCredential(acct, credential))
		return
	}
	printAccountTable(acct, credential)
}

// fetchAccountSummary loads the config, refuses to call the API without a
// billing credential, and fetches GET /account. It returns the tag of the
// credential authAuto used ("session" or "agent"). Split from cmdAccount so
// the guidance paths stay testable without os.Exit.
func fetchAccountSummary() (map[string]any, string, error) {
	cfg, err := loadConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Cold start: no config file at all. Point at both ways to get a
			// billing credential.
			return nil, "", errNoBillingCredentials
		}
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	credential := credentialLabel(cfg)
	if credential == "" {
		return nil, "", errNoBillingCredentials
	}
	acct, err := newBillingClient(cfg).Account()
	if err != nil {
		return nil, "", err
	}
	return acct, credential, nil
}

// credentialLabel names the stored credential authAuto selects: a saved human
// session wins over an agent key (billing.go), and "" means neither is stored.
func credentialLabel(cfg *Config) string {
	switch {
	case cfg.BillingSession != "":
		return "session"
	case cfg.BillingAgentKey != "":
		return "agent"
	default:
		return ""
	}
}

// credentialText is the human-readable form of a credentialLabel value.
func credentialText(credential string) string {
	if credential == "agent" {
		return "agent key"
	}
	return credential
}

// accountWithCredential copies the server object and adds the credential tag
// for --json. Server fields pass through untouched (balance_usd stays a string,
// nulls stay null); the credential's value is never added.
func accountWithCredential(acct map[string]any, credential string) map[string]any {
	out := make(map[string]any, len(acct)+1)
	for k, v := range acct {
		out[k] = v
	}
	out["credential"] = credential
	if v := buildTrialView(acct); v != nil {
		out["trial"] = v
	}
	honestEgressJSON(out, acct)
	return out
}

// accountBalanceUSD renders balance_usd for the human tables. FastAPI sends a
// JSON number: it is formatted to exactly two decimals, so 7.5 renders as
// 7.50 and large balances never fall into scientific notation. A quoted
// decimal is a legacy/defensive shape and passes through as sent; missing and
// null fields render as "".
func accountBalanceUSD(acct map[string]any, key string) string {
	v, ok := acct[key]
	if !ok || v == nil {
		return ""
	}
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', 2, 64)
	case int:
		return strconv.FormatFloat(float64(n), 'f', 2, 64)
	case int64:
		return strconv.FormatFloat(float64(n), 'f', 2, 64)
	case string:
		// moneyd serialises Decimal balances as strings ("0", "0.05"). Render
		// them exactly like numbers so the table and --json agree.
		if f, err := strconv.ParseFloat(n, 64); err == nil && (!strings.Contains(n, ".") || len(n)-strings.Index(n, ".")-1 <= 2) {
			return strconv.FormatFloat(f, 'f', 2, 64)
		}
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}

// displayUSD shortens an exact decimal string for people: more than four
// decimals round to four ("0.04989375392" -> "0.0499"), anything shorter is
// left alone. --json output keeps the exact value.
func displayUSD(s string) string {
	i := strings.Index(s, ".")
	if i < 0 || len(s)-i-1 <= 4 {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	out := strconv.FormatFloat(f, 'f', 4, 64)
	for strings.HasSuffix(out, "0") && len(out)-strings.Index(out, ".")-1 > 2 {
		out = strings.TrimSuffix(out, "0")
	}
	if out == "0.00" && f > 0 {
		return "<0.0001"
	}
	return out
}

// printAccountTable renders the human dashboard. Missing and null fields render
// as an em dash rather than a Go formatting artifact.
func printAccountTable(acct map[string]any, credential string) {
	field := func(label, value string) {
		fmt.Printf("  %-14s %s\n", label+":", dashIfEmpty(value))
	}
	balance := displayUSD(accountBalanceUSD(acct, "balance_usd"))
	if balance != "" {
		balance = "$" + balance
	}
	field("Billing ID", accountString(acct, "billing_id"))
	field("Kind", accountString(acct, "kind"))
	field("Status", accountString(acct, "status"))
	field("Balance", balance)
	field("Plan", accountString(acct, "plan"))
	field("Period ends", periodEndText(acct))
	if _, ok := acct["allowance_egress_bytes"]; ok {
		if n, _ := accountUint64(acct, "allowance_egress_bytes"); n > 0 {
			field("Allowance", formatBytes(n)+" of downloads")
		}
	}
	field("Downloads", egressDisplay(acct))
	days := accountDaysLeft(acct)
	if days != "" {
		n, _ := accountUint64(acct, "days_left")
		days = "about " + humanDays(n) + " (estimate at today's storage charge for what you store now)"
	} else {
		days = daysLeftEmptyText(acct)
	}
	field("Days left", days)
	field("Credential", credentialText(credential))
	field("Top-up", accountString(acct, "topup_url"))
	if v := buildTrialView(acct); v != nil {
		field("Readiness", readinessLabel(v.State))
		fmt.Printf("  %-14s %s\n", "", v.Summary)
		fmt.Printf("  %-14s %s\n", "", v.UploadsLine)
	}
}

// dashIfEmpty renders missing/null fields as an em dash.
func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// accountBytes renders a byte-count field, or "" when it is absent.
func accountBytes(acct map[string]any, key string) string {
	n, ok := accountUint64(acct, key)
	if !ok {
		return ""
	}
	return formatBytes(n)
}

// accountDaysLeft renders days_left, which moneyd nulls when burn is zero.
func accountDaysLeft(acct map[string]any) string {
	if n, ok := accountUint64(acct, "days_left"); ok {
		return strconv.FormatUint(n, 10)
	}
	return ""
}

// accountUint64 extracts a non-negative integer field. JSON numbers decode as
// float64; quoted integers are accepted defensively (moneyd renders decimals
// such as balance_usd as strings).
func accountUint64(acct map[string]any, key string) (uint64, bool) {
	v, ok := acct[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case string:
		parsed, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
