package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Free-trial and balance presentation (v1.11.4, TT retest 3).
//
// account, status, usage and signup used to print moneyd's readiness message
// verbatim, which read "$0.00" next to "top up", or "$0.05 left" next to "your
// first upload is free". The CLI now derives one explicit view from the
// structured fields of GET /account (readiness.state/can_upload/free_trial,
// balance_usd, days_left) and every command renders the same lines from it.

// trialCreditUSDDefault is the one-time free-trial credit (moneyd rate card
// promo_001), used only when the server does not report trial_credit_usd.
const trialCreditUSDDefault = "0.05"

// topUpHint is the plain top-up instruction shown when an upload is blocked.
const topUpHint = "tessera topup 5 (crypto minimum about $0.38 in USDC on Base, more for other coins; live list: tessera topup --minimums)"

// trialView is the consistent trial/balance state. It is also emitted as the
// "trial" object of account/status/usage --json.
type trialView struct {
	State         string  `json:"state"`
	FreeTrial     bool    `json:"free_trial"`
	CreditUSD     string  `json:"trial_credit_usd,omitempty"`
	CreditPosted  bool    `json:"trial_credit_posted"`
	BalanceUSD    string  `json:"balance_usd"`
	CanUpload     bool    `json:"can_upload"`
	TopUpRequired bool    `json:"top_up_required"`
	DaysLeft      *uint64 `json:"days_left_estimate"`
	Summary       string  `json:"summary"`
	UploadsLine   string  `json:"uploads"`
}

func readinessMap(acct map[string]any) map[string]any {
	r, _ := acct["readiness"].(map[string]any)
	return r
}

// buildTrialView returns nil when the response carries no readiness block.
func buildTrialView(acct map[string]any) *trialView {
	r := readinessMap(acct)
	if r == nil {
		return nil
	}
	v := &trialView{}
	v.State, _ = r["state"].(string)
	v.FreeTrial, _ = r["free_trial"].(bool)
	if b, ok := r["can_upload"].(bool); ok {
		v.CanUpload = b
	} else {
		v.CanUpload = v.State == "funded" || v.State == "free_trial" || v.State == "free_trial_active"
	}
	v.TopUpRequired, _ = r["top_up_required"].(bool)
	if v.State == "free_trial" || v.State == "free_trial_active" {
		v.FreeTrial = true
	}
	v.BalanceUSD = accountBalanceUSD(acct, "balance_usd")
	if v.BalanceUSD == "" {
		v.BalanceUSD = "0.00"
	}
	balance, _ := strconv.ParseFloat(v.BalanceUSD, 64)
	if v.FreeTrial {
		v.CreditUSD = trialCreditUSDDefault
		if s := accountBalanceUSD(r, "trial_credit_usd"); s != "" {
			v.CreditUSD = s
		}
		if posted, ok := r["trial_credit_posted"].(bool); ok {
			v.CreditPosted = posted
		} else {
			v.CreditPosted = v.State != "free_trial" || balance > 0
		}
	}
	if n, ok := accountUint64(acct, "days_left"); ok {
		v.DaysLeft = &n
	}
	v.Summary, v.UploadsLine = trialLines(v, readinessMessage(r))
	return v
}

func readinessMessage(r map[string]any) string {
	m, _ := r["message"].(string)
	return m
}

// trialLines renders the two plain lines: what the account has, and whether
// another upload is allowed right now.
func trialLines(v *trialView, serverMsg string) (summary, uploads string) {
	credit := "$" + v.CreditUSD
	switch {
	case v.FreeTrial && v.State == "free_trial" && !v.CreditPosted:
		summary = "Free trial: new accounts get a one-time " + credit + " credit and Tessera pays the storage hosts for you. " +
			"The credit is added automatically within about a minute of signup (at the latest with your first upload); until then the balance reads $0.00."
	case v.FreeTrial && v.CanUpload:
		summary = fmt.Sprintf("Free trial: $%s of your one-time %s credit left. Your uploads, stored files and downloads are charged against it; Tessera pays the storage hosts.", displayUSD(v.BalanceUSD), credit)
	case v.FreeTrial:
		summary = "Free trial: your one-time " + credit + " credit is used up."
	case v.State == "funded":
		summary = "Funded: balance $" + displayUSD(v.BalanceUSD) + "."
	default:
		summary = serverMsg
	}
	switch {
	case v.CanUpload && v.FreeTrial:
		uploads = "Uploads: allowed now, no top-up needed"
		if v.CreditPosted && v.DaysLeft != nil {
			uploads += fmt.Sprintf(", while the credit lasts (%s)", daysLeftPhrase(*v.DaysLeft))
		}
		uploads += ". Top up any time to keep storing beyond the trial: tessera topup 5"
	case v.CanUpload:
		uploads = "Uploads: allowed."
	case v.TopUpRequired:
		uploads = "Uploads: blocked until you top up: " + topUpHint
	default:
		uploads = "Uploads: not possible right now."
		if serverMsg != "" && summary != serverMsg {
			uploads += " " + serverMsg
		}
	}
	return summary, uploads
}

// daysLeftPhrase labels days_left as what it is: an estimate at the current
// storage charge for what is stored now.
func daysLeftPhrase(days uint64) string {
	return "about " + humanDays(days) + " at today's storage charge for what you store now; more uploads or downloads use it faster"
}

func humanDays(days uint64) string {
	s := strconv.FormatUint(days, 10) + " days"
	if days >= 730 {
		s += fmt.Sprintf(" (~%d years)", uint64(math.Round(float64(days)/365)))
	}
	return s
}

// printTrialBlock prints the view with a fixed indent and label width.
func printTrialBlock(v *trialView, indent string, labelWidth int) {
	if v == nil {
		return
	}
	label := func(l, text string) {
		fmt.Printf("%s%-*s %s\n", indent, labelWidth, l, text)
	}
	label("Status:", readinessLabel(v.State))
	label("", v.Summary)
	label("", v.UploadsLine)
}

// ---------------------------------------------------------------- egress

// egressNote replaces the old "egress used" byte count. The client-reported
// egress feed is retired, so egress_used_bytes is always 0; downloads are
// billed from measured host spend instead, as USD debits.
const egressNote = "charged from measured host spend (checked about every 5 minutes) and taken from your balance; not reported as a byte count"

// egressMeasuredInBytes reports whether the server still sends a meaningful
// egress byte count (a server that does says so with egress_measurement).
func egressMeasuredInBytes(acct map[string]any) bool {
	m, _ := acct["egress_measurement"].(string)
	return m == "bytes"
}

// egressDisplay is the human value for the downloads row.
func egressDisplay(acct map[string]any) string {
	if egressMeasuredInBytes(acct) {
		return accountBytes(acct, "egress_used_bytes")
	}
	if usd := accountBalanceUSD(acct, "egress_billed_usd"); usd != "" {
		return "$" + displayUSD(usd) + " this period — " + egressNote
	}
	return egressNote
}

// honestEgressJSON nulls the retired byte counter in --json output and says
// why, so agents do not read a permanent 0 as "no downloads".
func honestEgressJSON(out map[string]any, acct map[string]any) {
	if egressMeasuredInBytes(acct) {
		return
	}
	if _, ok := out["egress_used_bytes"]; ok {
		out["egress_used_bytes"] = nil
	}
	out["egress_note"] = "downloads are " + egressNote
}

// ---------------------------------------------------------------- sizes

// sizeExplanation is the one-paragraph account of why network size is larger
// than your files (TT: "120 MiB network size for a 1.7 MB file").
var sizeExplanation = []string{
	"Why the sizes differ: files are stored in 40 MiB slabs. A slab is billed whole,",
	"so a small file uploaded on its own still fills one slab (uploading a folder or",
	"using 'tessera sync' packs small files into shared slabs). Each slab is split",
	"into 30 pieces of 4 MiB on 30 different hosts, any 10 of which rebuild it, so",
	"network size is 3× stored size.",
}

func printSizeExplanation(indent string) {
	for _, l := range sizeExplanation {
		fmt.Println(indent + l)
	}
}

// slabCountText renders "1 slab" / "3 slabs".
func slabCountText(acct map[string]any) string {
	n, ok := accountUint64(acct, "open_slabs")
	if !ok {
		return ""
	}
	if n == 1 {
		return "1 slab"
	}
	return strconv.FormatUint(n, 10) + " slabs"
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// localStamp renders a time in the machine's local zone with the zone named
// ("2026-10-30 15:06 +07"), so listings never show unlabelled UTC (TT retest 3).
func localStamp(t time.Time, seconds bool) string {
	layout := "2006-01-02 15:04"
	if seconds {
		layout += ":05"
	}
	return t.Local().Format(layout + " MST")
}

// periodEndText explains the empty period end of a pay-as-you-go account
// instead of a bare "—".
func periodEndText(acct map[string]any) string {
	if v := accountString(acct, "period_end"); v != "" {
		return v
	}
	if strings.EqualFold(accountString(acct, "plan"), "payg") {
		return "none (pay as you go: charges come off the balance as they happen)"
	}
	return ""
}

// daysLeftEmptyText says why there is no days-left estimate yet.
func daysLeftEmptyText(acct map[string]any) string {
	if _, ok := accountUint64(acct, "days_left"); ok {
		return ""
	}
	if n, ok := accountUint64(acct, "stored_bytes"); ok && n == 0 {
		return "no estimate yet (nothing stored; storage is the daily charge it is based on)"
	}
	return ""
}
