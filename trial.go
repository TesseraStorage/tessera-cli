package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Free-trial and balance presentation (Money-10; TT run 5).
//
// account, status, usage and signup used to print moneyd's readiness message
// verbatim, which read "$0.00" next to "top up", or "$0.05 left" next to "your
// first upload is free". The CLI now derives one explicit view from the
// structured fields of GET /account (readiness.state/can_upload/free_trial,
// balance_usd, days_left) and every command renders the same lines from it.

// trialCreditUSDDefault is the free-trial credit used when the server does not
// report trial_credit_usd. Since Money 10 (2026-11-03) the trial carries no
// credit: it is one 40 MiB block upload plus one download within 72 hours of
// signup at a $0.00 balance (Tessera pays the storage hosts).
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
	FileKeptUntil     string  `json:"file_kept_until,omitempty"` // ISO; alias of trial_ends_at (72h keep-until)
	TrialUploadUsed   bool    `json:"trial_upload_used,omitempty"`
	TrialDownloadUsed bool    `json:"trial_download_used,omitempty"`
	GraceEndsAt       string  `json:"grace_ends_at,omitempty"`
	// CreditDeprecated is set when trial_credit_usd is kept only for older
	// clients (Money-10 trial has no credit; balance stays $0.00).
	CreditDeprecated  bool    `json:"trial_credit_deprecated,omitempty"`
	Summary           string  `json:"summary"`
	UploadsLine       string  `json:"uploads"`
	ShortBanner       string  `json:"short_banner,omitempty"`
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
	credit := "$" + v.CreditUSD
	if v.FreeTrial && creditIsZero(v.CreditUSD) {
		return blockTrialLines(v, serverMsg)
	}
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

// blockTrialLines renders the Money-10 trial: one 40 MiB block upload and one
// download, free for 72 hours after signup, balance $0.00, no credit.
func blockTrialLines(v *trialView, serverMsg string) (summary, uploads string) {
	keptUntil := trialKeptUntilPhrase(v)
	switch {
	case v.State == "free_trial" && v.CanUpload:
		summary = "Free trial: one upload of up to 40 MiB (one block) and one download. " + keptUntil +
			" Tessera pays the storage hosts. Your balance stays $0.00. Top up any time to keep storing beyond the trial: tessera topup 1"
		uploads = "Uploads: allowed now, no top-up needed"
	case v.State == "free_trial_active":
		summary = "Free trial: your free upload is stored; your one free download is still available. " + keptUntil
		uploads = "Uploads: blocked until you top up: " + topUpHint
	default:
		summary = "Free trial used or ended (one upload and one download, 72 hours after signup)."
		if serverMsg != "" {
			summary = serverMsg
		}
		if v.CanUpload {
			uploads = "Uploads: allowed."
		} else {
			uploads = "Uploads: blocked until you top up: " + topUpHint
		}
	}
	return summary, uploads
}

// trialKeptUntilPhrase is the 72h keep-until line for trial accounts.
func trialKeptUntilPhrase(v *trialView) string {
	// Do not start with "Free trial:" — callers already prefix that; doubling
	// it produced "Free trial: … Free trial: file kept until …" (TT run 5 #10).
	if v != nil && v.TrialEndsAt != "" {
		if t, err := time.Parse(time.RFC3339, v.TrialEndsAt); err == nil {
			return "File kept until " + localStamp(t, false) + ", then deleted unless you top up."
		}
		return "File kept until " + v.TrialEndsAt + ", then deleted unless you top up."
	}
	return "Files are deleted 72 hours after signup unless you top up."
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
		return "Free trial active (1 upload + 1 download). Top up any time: tessera topup 1"
	case "free_trial_active":
		return "Free trial: upload used; 1 free download left. Top up to upload again: tessera topup 1"
	default:
		return "Free trial used — top up to continue: tessera topup 1"
	}
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
const egressNote = "charged from measured host spend (checked about every 5 minutes) and taken from your balance; trial downloads report actual bytes (not billed)"

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
					return formatBytes(n) + " (1 free trial download, not billed)"
				}
				if n, ok := accountUint64(acct, "raw_bytes"); ok && n > 0 {
					return formatBytes(n) + " (1 free trial download, not billed)"
				}
				return "1 free trial download (not billed)"
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
		return s + ") this period; measured from host spend about every 5 minutes"
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
						out["egress_note"] = "1 free trial download (not billed); bytes are the downloaded size"
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
				out["egress_note"] = "1 free trial download (not billed); bytes are the downloaded size"
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
// TT-nov5: for trial accounts prefer the keep-until phrase over a dash /
// "about 0 days"; for paid $0 accounts explain grace.
func daysLeftEmptyText(acct map[string]any) string {
	if n, ok := accountUint64(acct, "days_left"); ok && n > 0 {
		return ""
	}
	if r := readinessMap(acct); r != nil {
		if ft, _ := r["free_trial"].(bool); ft {
			ends, _ := r["trial_ends_at"].(string)
			if ends != "" {
				if t, err := time.Parse(time.RFC3339, ends); err == nil {
					return "free trial: file kept until " + localStamp(t, false) + ", then deleted unless you top up"
				}
				return "free trial: file kept until " + ends + ", then deleted unless you top up"
			}
			return "free trial: file kept for 72 hours after signup, then deleted unless you top up"
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
