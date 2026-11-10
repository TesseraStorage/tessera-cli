package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trialAcct(state string, balance string, canUpload bool, days any) map[string]any {
	return map[string]any{
		"balance_usd": balance,
		"days_left":   days,
		"readiness": map[string]any{
			"state": state, "message": "server text", "can_upload": canUpload,
			"top_up_required": !canUpload, "free_trial": true,
		},
	}
}

// Money 10: the trial is one 40 MiB block upload plus one download at $0.00,
// no credit; every state says so explicitly and consistently.
func TestTrialViewStates(t *testing.T) {
	v := buildTrialView(trialAcct("free_trial", "0", true, nil))
	if v.CreditPosted || !v.CanUpload || (v.CreditUSD != "" && v.CreditUSD != "0") || !v.CreditDeprecated ||
		!strings.Contains(v.Summary, "one upload of up to 40 MiB") ||
		!strings.Contains(v.Summary, "Balance stays $0.00") || !strings.HasPrefix(v.UploadsLine, "Uploads: allowed now") ||
		!strings.Contains(v.Summary, "tessera topup 5") {
		t.Fatalf("free_trial view: %+v", v)
	}
	if v.DaysLeft != nil {
		t.Fatalf("Money-10 trial must null days_left_estimate, got %v", *v.DaysLeft)
	}
	if v.Summary != technicalValidationSummary {
		t.Fatalf("Summary must be the locked technical-validation wording: %q", v.Summary)
	}
	if v.ShortBanner != "Technical validation available (1 upload + 1 download). Top up any time: tessera topup 5" {
		t.Fatalf("free_trial short banner: %+v", v)
	}
	if strings.Contains(v.Summary+v.UploadsLine, "credit") {
		t.Fatalf("block trial must not mention a credit: %+v", v)
	}
	v = buildTrialView(trialAcct("free_trial_active", "0", false, nil))
	if v.CanUpload || !strings.Contains(v.Summary, "Technical validation: upload used; your one download is still available") ||
		!strings.HasPrefix(v.UploadsLine, "Uploads: blocked until you top up") {
		t.Fatalf("active view: %+v", v)
	}
	if v.ShortBanner != "Technical validation: upload used; 1 download left. Top up to upload again: tessera topup 5" {
		t.Fatalf("active short banner: %q", v.ShortBanner)
	}
	v = buildTrialView(trialAcct("top_up_required", "0", false, nil))
	if v.CanUpload || v.Summary != "server text" || !strings.HasPrefix(v.UploadsLine, "Uploads: blocked until you top up") {
		t.Fatalf("used view: %+v", v)
	}
	// A server message in the retired wording is replaced by the CLI's own line.
	a := trialAcct("top_up_required", "0", false, nil)
	a["readiness"].(map[string]any)["message"] = "Free trial used or ended. Files are deleted 72 hours after signup."
	v = buildTrialView(a)
	if v.Summary != "Technical validation used (one upload and one download). Top up to continue: tessera topup 5" ||
		v.ShortBanner != "Technical validation used — top up to continue: tessera topup 5" {
		t.Fatalf("retired server wording leaked: %+v", v)
	}
}

// Wording pass 2026-11-06: no user-facing text names the retired trial, a
// timer or subscriptions, in any state.
func TestTechnicalValidationNoRetiredWording(t *testing.T) {
	bad := []string{"free trial", "foray", "72 hour", "kept until", "subscription", "one-time", "deleted"}
	for _, st := range []string{"free_trial", "free_trial_active", "top_up_required", "suspended"} {
		a := trialAcct(st, "0", st == "free_trial", nil)
		a["readiness"].(map[string]any)["trial_ends_at"] = "2026-11-08T09:00:00Z"
		a["readiness"].(map[string]any)["message"] = "Free trial ended 72 hours after signup."
		v := buildTrialView(a)
		text := strings.ToLower(v.Summary + " " + v.UploadsLine + " " + v.ShortBanner + " " + readinessLabel(st) + " " + daysLeftEmptyText(a))
		for _, b := range bad {
			if st == "suspended" && b == "deleted" {
				continue
			}
			if strings.Contains(text, b) {
				t.Fatalf("%s: %q contains %q", st, text, b)
			}
		}
	}
	label := readinessLabel("suspended")
	if !strings.Contains(label, "files deleted") || !strings.Contains(label, "top up to continue") {
		t.Fatalf("suspended label should say files deleted and top up to continue: %q", label)
	}
	h := strings.ToLower(usageText)
	for _, b := range []string{"free trial", "72 hours", "promo", "subscription"} {
		if strings.Contains(h, b) {
			t.Fatalf("help mentions %q", b)
		}
	}
}

// A server that still reports a trial credit keeps the credit wording.
func TestTrialViewLegacyCredit(t *testing.T) {
	a := trialAcct("free_trial_active", "0.25", true, float64(4472))
	a["readiness"].(map[string]any)["trial_credit_usd"] = "0.25"
	a["readiness"].(map[string]any)["trial_credit_posted"] = true
	v := buildTrialView(a)
	if !v.CreditPosted || !strings.Contains(v.Summary, "Technical validation: $0.25 balance left") || strings.Contains(v.Summary, "one-time") ||
		!strings.Contains(v.UploadsLine, "allowed now") || !strings.Contains(v.UploadsLine, daysLeftOverCap) {
		t.Fatalf("legacy active view: %+v", v)
	}
}

// #9: a working free trial never produces the per-command notice.
func TestReadinessNoticeSilentDuringTrial(t *testing.T) {
	for _, st := range []string{"free_trial", "free_trial_active", "funded"} {
		if readinessNeedsAction(st) {
			t.Fatalf("%s should be silent", st)
		}
	}
	for _, cmd := range []string{"list", "ls", "whoami", "usage", "versions", "find"} {
		if readinessNoticeCommands[cmd] {
			t.Fatalf("%s must not print the readiness notice", cmd)
		}
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := readinessCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	data, _ := json.Marshal(readinessCache{CheckedAt: time.Now(), State: "free_trial_active"})
	_ = os.WriteFile(p, data, 0o600)
	if n := readinessNotice(&Config{BillingAgentKey: "tsk_test"}); n != "" {
		t.Fatalf("trial notice printed: %q", n)
	}
}

// #4: no indexd jargon in user-facing text.
func TestReadyNoteNoJargon(t *testing.T) {
	for _, s := range []string{readyNote(false), readyNoteFor(false, "funded")} {
		if strings.Contains(s, "indexd") || strings.Contains(s, "repair-redundancy") {
			t.Fatalf("jargon: %q", s)
		}
	}
}

// #11: list times are local and carry the zone.
func TestLocalStampNamesZone(t *testing.T) {
	loc := time.FixedZone("+07", 7*3600)
	old := time.Local
	time.Local = loc
	defer func() { time.Local = old }()
	got := localStamp(time.Date(2026, 10, 30, 8, 6, 0, 0, time.UTC), false)
	if got != "2026-10-30 15:06 +07" {
		t.Fatalf("got %q", got)
	}
}

// #5: binary units are labelled as binary units.
func TestFormatBytesBinaryLabels(t *testing.T) {
	if formatBytes(40<<20) != "40.00 MiB" || formatBytes(120<<20) != "120.00 MiB" || formatBytes(1536) != "1.5 KiB" {
		t.Fatalf("labels: %s %s %s", formatBytes(40<<20), formatBytes(120<<20), formatBytes(1536))
	}
}

func TestEgressDisplayBilledBlocks(t *testing.T) {
	acct := map[string]any{
		"egress_measurement":   "host_spend",
		"egress_used_bytes":    float64(3 * 41943040),
		"egress_billed_blocks": float64(3),
		"egress_billed_usd":    "0.0012582912",
	}
	got := egressDisplay(acct)
	for _, want := range []string{"3 blocks of 40 MiB", "$0.001258", "billed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("egressDisplay = %q, missing %q", got, want)
		}
	}
	out := map[string]any{"egress_used_bytes": acct["egress_used_bytes"]}
	honestEgressJSON(out, acct)
	if out["egress_used_bytes"] == nil || out["egress_billed_blocks"] != uint64(3) {
		t.Fatalf("honestEgressJSON nulled billed bytes: %#v", out)
	}
}

func TestSlabCountTextLag(t *testing.T) {
	acct := map[string]any{"open_slabs": float64(0), "stored_bytes": float64(40 * 1024 * 1024)}
	if got := slabCountText(acct); got != "1 slab" {
		t.Fatalf("lagging meter: got %q", got)
	}
	acct2 := map[string]any{"open_slabs": float64(0), "stored_bytes": float64(0)}
	if got := slabCountText(acct2); got != "0 slabs" {
		t.Fatalf("empty store: got %q", got)
	}
}

func TestEgressDisplayTrialDownload(t *testing.T) {
	acct := map[string]any{
		"egress_billed_blocks": float64(0),
		"egress_billed_usd":    "0",
		"readiness": map[string]any{
			"state": "free_trial_active", "free_trial": true,
			"trial_download_used": true, "can_upload": false,
		},
	}
	if got := egressDisplay(acct); got != "1 technical-validation download (not billed)" {
		t.Fatalf("trial download display: %q", got)
	}
}

func TestTrialKeptUntilNoDoublePrefix(t *testing.T) {
	a := trialAcct("free_trial", "0", true, float64(0))
	a["readiness"].(map[string]any)["trial_ends_at"] = "2026-11-08T09:00:00Z"
	a["days_left"] = float64(0)
	v := buildTrialView(a)
	if v.FileKeptUntil != "2026-11-08T09:00:00Z" {
		t.Fatalf("FileKeptUntil: %q", v.FileKeptUntil)
	}
	if v.DaysLeft != nil {
		t.Fatalf("days_left must be null on trial, got %v", *v.DaysLeft)
	}
	if strings.Count(v.Summary, "Technical validation:") != 1 {
		t.Fatalf("double prefix: %q", v.Summary)
	}
	// The keep-until time stays in JSON only; it is never printed.
	if strings.Contains(v.Summary, "kept until") || strings.Contains(v.Summary, "2026-11-08") {
		t.Fatalf("keep-until leaked into summary: %q", v.Summary)
	}
}

// TestPublicReadinessJSON: --json keeps readiness keys but never passes on a
// server message with retired wording.
func TestPublicReadinessJSON(t *testing.T) {
	r := map[string]any{"state": "top_up_required", "free_trial": true,
		"message": "Free trial used (one upload and one download)."}
	acct := map[string]any{"readiness": r}
	got, ok := publicReadinessJSON(r, buildTrialView(acct)).(map[string]any)
	if !ok {
		t.Fatalf("not a map")
	}
	if len(got) != len(r) || got["state"] != "top_up_required" {
		t.Errorf("keys/values changed: %v", got)
	}
	msg, _ := got["message"].(string)
	if strings.Contains(strings.ToLower(msg), "trial") || !strings.Contains(msg, "Technical validation") {
		t.Errorf("message = %q", msg)
	}
	if r["message"] != "Free trial used (one upload and one download)." {
		t.Errorf("input map was mutated")
	}
	clean := map[string]any{"state": "funded", "message": "Ready."}
	if g := publicReadinessJSON(clean, nil).(map[string]any); g["message"] != "Ready." {
		t.Errorf("clean message changed: %v", g)
	}
}
