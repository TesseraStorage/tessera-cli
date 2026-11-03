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
	if v.CreditPosted || !v.CanUpload || v.CreditUSD != "0" || !strings.Contains(v.Summary, "one upload of up to 40 MiB") ||
		!strings.Contains(v.Summary, "balance stays $0.00") || !strings.HasPrefix(v.UploadsLine, "Uploads: allowed now") ||
		!strings.Contains(v.UploadsLine, "tessera topup 1") {
		t.Fatalf("free_trial view: %+v", v)
	}
	if strings.Contains(v.Summary+v.UploadsLine, "credit") {
		t.Fatalf("block trial must not mention a credit: %+v", v)
	}
	v = buildTrialView(trialAcct("free_trial_active", "0", false, nil))
	if v.CanUpload || !strings.Contains(v.Summary, "one free download is still available") ||
		!strings.Contains(v.Summary, "deleted 72 hours after signup") || !strings.HasPrefix(v.UploadsLine, "Uploads: blocked until you top up") {
		t.Fatalf("active view: %+v", v)
	}
	v = buildTrialView(trialAcct("top_up_required", "0", false, nil))
	if v.CanUpload || v.Summary != "server text" || !strings.HasPrefix(v.UploadsLine, "Uploads: blocked until you top up") {
		t.Fatalf("used view: %+v", v)
	}
}

// A server that still reports a trial credit keeps the credit wording.
func TestTrialViewLegacyCredit(t *testing.T) {
	a := trialAcct("free_trial_active", "0.25", true, float64(4472))
	a["readiness"].(map[string]any)["trial_credit_usd"] = "0.25"
	a["readiness"].(map[string]any)["trial_credit_posted"] = true
	v := buildTrialView(a)
	if !v.CreditPosted || !strings.Contains(v.Summary, "$0.25 of your one-time $0.25 credit left") ||
		!strings.Contains(v.UploadsLine, "allowed now") || !strings.Contains(v.UploadsLine, "about 4472 days (~12 years)") {
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
