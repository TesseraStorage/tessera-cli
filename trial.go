package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Technical-validation and balance presentation (Money-10; TT run 5).
//
// account, status, usage and signup used to print moneyd's readiness message
// verbatim, which read "$0.00" next to "top up", or "$0.05 left" next to "your
// first upload is free". The CLI now derives one explicit view from the
// structured fields of GET /account (readiness.state/can_upload/free_trial,
// balance_usd, days_left) and every command renders the same lines from it.

// trialCreditUSDDefault is the legacy trial credit used when the server does
// not report trial_credit_usd. Since Money 10 (2026-11-03) the zero-balance
// allowance ("technical validation") carries no credit: one upload of up to
// 40 MiB and one download at a $0.00 balance (Tessera covers the hosts).
// User-facing text never names a timer for it.
const trialCreditUSDDefault = "0"

// topUpHint is the plain top-up instruction shown when an upload is blocked.
const topUpHint = "tessera topup 1 (crypto minimum about $0.38 in USDC on Base, more for other coins; live list: tessera topup --minimums)"

// trialView is the consistent trial/balance state. It is also emitted as the
// "trial" object of account/status/usage --json.
type trialView struct {
	State             string  `json:"state"`
	FreeTrial         bool    `json:"free_trial"`
	CreditUSD         string  `json:"trial_credit_usd,omitempty"`
	CreditPosted      bool    `json:"trial_credit_posted"`
	BalanceUSD        string  `json:"balance_usd"`
	CanUpload         bool    `json:"can_upload"`
	TopUpRequired     bool    `json:"top_up_required"`
	DaysLeft          *uint64 `json:"days_left_estimate"`
	TrialEndsAt       string  `json:"trial_ends_at,omitempty"`
	FileKeptUntil     string  `json:"file_kept_until,omitempty"` // ISO; alias of trial_ends_at (JSON only; never printed)
	TrialUploadUsed   bool    `json:"trial_upload_used,omitempty"`
	TrialDownloadUsed bool    `json:"trial_download_used,omitempty"`
	GraceEndsAt       string  `json:"grace_ends_at,omitempty"`
	// CreditDeprecated is set when trial_credit_usd is kept only for older
	// clients (Money-10 trial has no credit; balance stays $0.00).
	CreditDeprecated bool   `json:"trial_credit_deprecated,omitempty"`
	Summary          string `json:"summary"`
	UploadsLine      string `json:"uploads"`
	ShortBanner      string `json:"short_banner,omitempty"`
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
	if v.FreeTrial {
		// Money-10: no credit. Keep trial_credit_usd out of JSON when it is
		// "0"/absent so agents do not treat it as a spendable balance, but set
		// CreditDeprecated so older docs that mention the field still make sense.
		rawCredit := accountBalanceUSD(r, "trial_credit_usd")
		if rawCredit != "" && !creditIsZero(rawCredit) {
			v.CreditUSD = rawCredit
		}
		if posted, ok := r["trial_credit_posted"].(bool); ok {
			v.CreditPosted = posted
		}
		if creditIsZero(v.CreditUSD) {
			v.CreditDeprecated = true
			v.CreditPosted = false
			v.CreditUSD = "" // omitempty
		}
	}
	// Money-10 zero-credit trial: days_left from burn/balance is meaningless
	// at $0 (reads as "deleted now"). Prefer null + file_kept_until.
	// Legacy credited trials still expose days_left_estimate.
	if v.FreeTrial && creditIsZero(v.CreditUSD) {
		v.DaysLeft = nil
	} else if n, ok := accountUint64(acct, "days_left"); ok {
		v.DaysLeft = &n
	}
	v.TrialEndsAt, _ = r["trial_ends_at"].(string)
	if v.TrialEndsAt == "" {
		if s := accountString(acct, "file_kept_until"); s != "" {
			v.TrialEndsAt = s
		} else if s := accountString(acct, "trial_expires_at"); s != "" {
			v.TrialEndsAt = s
		}
	}
	v.FileKeptUntil = v.TrialEndsAt
	v.GraceEndsAt, _ = r["grace_ends_at"].(string)
	if b, ok := r["trial_upload_used"].(bool); ok {
		v.TrialUploadUsed = b
	}
	if b, ok := r["trial_download_used"].(bool); ok {
		v.TrialDownloadUsed = b
	}
	v.Summary, v.UploadsLine = trialLines(v, readinessMessage(r))
	v.ShortBanner = shortTrialBanner(v)
	return v
}

func readinessMessage(r map[string]any) string {
	m, _ := r["message"].(string)
	return m
}

// trialLines renders the two plain lines: what the account has, and whether
// another upload is allowed right now.
func trialLines(v *trialView, serverMsg string) (summary, uploads string) {
	if v.FreeTrial && creditIsZero(v.CreditUSD) {
		return blockTrialLines(v, serverMsg)
	}
	if v.FreeTrial {
		// Legacy server that still posts a credit: state the balance plainly,
		// without the retired "one-time credit" / "free trial" sentences.
		summary = fmt.Sprintf("Technical validation: $%s balance left. Uploads, stored files and downloads are charged against it; Tessera covers the hosts.", displayUSD(v.BalanceUSD))
		switch {
		case v.CanUpload:
			uploads = "Uploads: allowed now, no top-up needed"
			if v.DaysLeft != nil {
				uploads += fmt.Sprintf(", while the balance lasts (%s)", daysLeftPhrase(*v.DaysLeft))
			}
		default:
			uploads = "Uploads: blocked until you top up: " + topUpHint
		}
		return summary, uploads
	}
	serverMsg = publicServerMessage(serverMsg)
	switch {
	case v.State == "funded":
		summary = "Funded: balance $" + displayUSD(v.BalanceUSD) + "."
	default:
		summary = serverMsg
	}
	switch {
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

func creditIsZero(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return s == "" || (err == nil && f == 0)
}

// technicalValidationSummary is the locked wording for the zero-balance
// allowance (CLI wording pass, 2026-11-06). No timer, no "free trial".
const technicalValidationSummary = "Technical validation: one upload of up to 40 MiB and one download. " +
	"Balance stays $0.00. Tessera covers the hosts. After that, top up to continue: tessera topup 1"

// blockTrialLines renders technical validation: one upload of up to 40 MiB
// and one download, balance $0.00, Tessera covers the hosts.
func blockTrialLines(v *trialView, serverMsg string) (summary, uploads string) {
	switch {
	case v.State == "free_trial" && v.CanUpload:
		summary = technicalValidationSummary
		uploads = "Uploads: allowed now, no top-up needed"
	case v.State == "free_trial_active":
		summary = "Technical validation: upload used; your one download is still available. " +
			"Balance stays $0.00. Tessera covers the hosts. After that, top up to continue: tessera topup 1"
		uploads = "Uploads: blocked until you top up: " + topUpHint
	default:
		summary = "Technical validation used (one upload and one download). Top up to continue: tessera topup 1"
		if m := publicServerMessage(serverMsg); m != "" {
			summary = m
		}
		if v.CanUpload {
			uploads = "Uploads: allowed."
		} else {
			uploads = "Uploads: blocked until you top up: " + topUpHint
		}
	}
	return summary, uploads
}

// publicServerMessage drops a server readiness message that still uses the
// retired wording (free trial, foray, a timer, subscriptions), so the CLI's own
// technical-validation lines are shown instead. Other messages pass through.
func publicServerMessage(m string) string {
	l := strings.ToLower(m)
	for _, bad := range []string{"trial", "foray", "72 hour", "72h", "subscription", "starter", "renew", "kept until", "deleted"} {
		if strings.Contains(l, bad) {
			return ""
		}
	}
	return m
}

// shortTrialBanner is one short line for commands other than account/status.
func shortTrialBanner(v *trialView) string {
	if v == nil {
		return ""
	}
	if !v.FreeTrial {
		if v.TopUpRequired {
			return "Top up to continue: " + topUpHint
		}
		return ""
	}
	switch v.State {
	case "free_trial":
		return "Technical validation available (1 upload + 1 download). Top up any time: tessera topup 1"
	case "free_trial_active":
		return "Technical validation: upload used; 1 download left. Top up to upload again: tessera topup 1"
	default:
		return "Technical validation used — top up to continue: tessera topup 1"
	}
}

// daysLeftPhrase labels days_left as what it is: an estimate at the current
// storage charge for what is stored now.
// tenYearsDays is where the days-left estimate stops printing a day count.
// A large balance against a tiny storage charge would otherwise read as
// tens of thousands of days.
const tenYearsDays = 10 * 365

// daysLeftOverCap is the owner-approved wording once the estimate exceeds
// ten years. Transfers are not part of the storage-charge figure.
const daysLeftOverCap = "more than 10 years at today's storage charge; transfers not included"

func daysLeftOverTenYears(days uint64) bool {
	return days > tenYearsDays
}

func daysLeftPhrase(days uint64) string {
	if daysLeftOverTenYears(days) {
		return daysLeftOverCap
	}
	return "about " + humanDays(days) + " at today's storage charge for what you store now; more uploads or downloads use it faster"
}

// daysLeftDetail is the account/usage wording for a positive days_left.
func daysLeftDetail(days uint64) string {
	if daysLeftOverTenYears(days) {
		return daysLeftOverCap
	}
	return "about " + humanDays(days) + " (estimate at today's storage charge for what you store now)"
}

// daysLeftField is the human days-left cell shared by account and usage.
func daysLeftField(acct map[string]any) string {
	if n, ok := accountUint64(acct, "days_left"); ok && n > 0 {
		return daysLeftDetail(n)
	}
	if alt := daysLeftEmptyText(acct); alt != "" {
		return alt
	}
	if n, ok := accountUint64(acct, "days_left"); ok {
		return daysLeftDetail(n)
	}
	return ""
}

// lastMeteredPresent reports whether a last_metered_at value is worth showing.
// A missing field, JSON null, or empty string is absent; the command still
// works on servers that have not added the field yet.
func lastMeteredPresent(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return false
	}
	return true
}

// lastMeteredLabel formats last_metered_at for the usage billing section.
// The raw value is kept for --json; this is only the human line.
func lastMeteredLabel(acct map[string]any) (string, bool) {
	v, ok := acct["last_metered_at"]
	if !ok || !lastMeteredPresent(v) {
		return "", false
	}
	s, isStr := v.(string)
	if !isStr {
		s = fmt.Sprint(v)
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		return t.In(time.Local).Format("2006-01-02 15:04 MST"), true
	}
	return s, true
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
	// TT-nov5 E: skip UploadsLine when Summary already carries the same
	// "blocked until you top up" wording (server readiness message often does).
	if v.UploadsLine != "" && !strings.Contains(v.Summary, "blocked until you top up") {
		label("", v.UploadsLine)
	}
}

// ---------------------------------------------------------------- egress

// egressNote replaces the old "egress used" byte count. The client-reported
// egress feed is retired, so egress_used_bytes is always 0; downloads are
// billed from measured host spend instead, as USD debits.
const egressNote = "charged from measured host spend (checked within about 15 minutes) and taken from your balance; the technical-validation download reports actual bytes (not billed)"

// egressMeasuredInBytes reports whether the server still sends a meaningful
// egress byte count (a server that does says so with egress_measurement).
func egressMeasuredInBytes(acct map[string]any) bool {
	m, _ := acct["egress_measurement"].(string)
	return m == "bytes"
}

// egressDisplay is the human value for the downloads row.
func egressDisplay(acct map[string]any) string {
	if r := readinessMap(acct); r != nil {
		if used, _ := r["trial_download_used"].(bool); used {
			blocks, hasBlocks := accountUint64(acct, "egress_billed_blocks")
			usd := accountBalanceUSD(acct, "egress_billed_usd")
			if (!hasBlocks || blocks == 0) && (usd == "" || creditIsZero(usd)) {
				if n, ok := accountUint64(acct, "egress_used_bytes"); ok && n > 0 {
					return formatBytes(n) + " (1 technical-validation download, not billed)"
				}
				if n, ok := accountUint64(acct, "raw_bytes"); ok && n > 0 {
					return formatBytes(n) + " (1 technical-validation download, not billed)"
				}
				return "1 technical-validation download (not billed)"
			}
		}
	}
	if egressMeasuredInBytes(acct) {
		return accountBytes(acct, "egress_used_bytes")
	}
	if blocks, ok := accountUint64(acct, "egress_billed_blocks"); ok {
		// Money-10 servers: egress_used_bytes is exactly the billed 40 MiB
		// blocks, so show bytes, blocks and dollars together.
		s := accountBytes(acct, "egress_used_bytes") + " billed (" + strconv.FormatUint(blocks, 10) + " blocks of 40 MiB"
		if usd := accountBalanceUSD(acct, "egress_billed_usd"); usd != "" {
			s += ", $" + displayUSD(usd)
		}
		return s + ") this period; measured from host spend within about 15 minutes"
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
	if blocks, ok := accountUint64(acct, "egress_billed_blocks"); ok {
		out["egress_billed_blocks"] = blocks
		out["egress_billed_usd"] = acct["egress_billed_usd"]
		// Trial download bytes may be reported even when billed blocks are 0.
		if blocks == 0 {
			if r := readinessMap(acct); r != nil {
				if used, _ := r["trial_download_used"].(bool); used {
					if n, ok := accountUint64(acct, "egress_used_bytes"); ok && n > 0 {
						out["egress_used_bytes"] = n
						out["egress_note"] = "1 technical-validation download (not billed); bytes are the downloaded size"
						return
					}
				}
			}
		}
		return
	}
	if r := readinessMap(acct); r != nil {
		if used, _ := r["trial_download_used"].(bool); used {
			if n, ok := accountUint64(acct, "egress_used_bytes"); ok && n > 0 {
				out["egress_used_bytes"] = n
				out["egress_note"] = "1 technical-validation download (not billed); bytes are the downloaded size"
				return
			}
		}
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
// TT-nov5: never show "0 slabs" when stored_bytes > 0 (sector-meter lag);
// derive ceil(stored_bytes / 40 MiB) until open_slabs catches up.
func slabCountText(acct map[string]any) string {
	n, ok := accountUint64(acct, "open_slabs")
	if !ok {
		n = 0
	}
	if n == 0 {
		if stored, sok := accountUint64(acct, "stored_bytes"); sok && stored > 0 {
			const mib40 = uint64(40 * 1024 * 1024)
			n = (stored + mib40 - 1) / mib40
			if n == 0 {
				n = 1
			}
		}
	}
	if n == 0 {
		if !ok {
			return ""
		}
		return "0 slabs"
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
// TT-nov5: technical-validation accounts get a plain "not applicable" instead
// of a dash / "about 0 days" (no timer is shown); paid $0 accounts explain grace.
func daysLeftEmptyText(acct map[string]any) string {
	if n, ok := accountUint64(acct, "days_left"); ok && n > 0 {
		return ""
	}
	if r := readinessMap(acct); r != nil {
		if ft, _ := r["free_trial"].(bool); ft {
			return "not applicable (technical validation; balance stays $0.00)"
		}
		if grace, _ := r["grace_ends_at"].(string); grace != "" {
			if t, err := time.Parse(time.RFC3339, grace); err == nil {
				return "balance $0: retrieval grace until " + localStamp(t, false) + "; top up to keep storing"
			}
			return "balance $0: retrieval grace until " + grace + "; top up to keep storing"
		}
	}
	bal := accountBalanceUSD(acct, "balance_usd")
	if creditIsZero(bal) {
		if n, ok := accountUint64(acct, "stored_bytes"); ok && n > 0 {
			return "balance $0: top up to keep storing (a short retrieval grace may still apply)"
		}
	}
	if _, ok := accountUint64(acct, "days_left"); ok {
		return ""
	}
	if n, ok := accountUint64(acct, "stored_bytes"); ok && n == 0 {
		return "no estimate yet (nothing stored; storage is the daily charge it is based on)"
	}
	return ""
}

// publicReadinessJSON returns the readiness block for --json output. Keys and
// every other value pass through untouched; only a server "message" that still
// carries retired wording (see publicServerMessage) is replaced by the CLI's
// own summary, so --json never says what the human text no longer says.
func publicReadinessJSON(r any, v *trialView) any {
	m, ok := r.(map[string]any)
	if !ok {
		return r
	}
	msg, isStr := m["message"].(string)
	if !isStr || msg == "" || publicServerMessage(msg) != "" {
		return r
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	out["message"] = ""
	if v != nil {
		out["message"] = v.Summary
	}
	return out
}
