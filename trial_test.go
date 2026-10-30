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

// TT retest 3 #1: $0.00 before the credit, $0.05 after, and whether another
// upload is allowed, all said explicitly and consistently.
func TestTrialViewStates(t *testing.T) {
	v := buildTrialView(trialAcct("free_trial", "0", true, nil))
	if v.CreditPosted || !v.CanUpload || !strings.Contains(v.Summary, "one-time $0.05 credit") ||
		!strings.Contains(v.Summary, "balance reads $0.00") || !strings.HasPrefix(v.UploadsLine, "Uploads: allowed now") {
		t.Fatalf("free_trial view: %+v", v)
	}
	v = buildTrialView(trialAcct("free_trial_active", "0.05", true, float64(4472)))
	if !v.CreditPosted || !strings.Contains(v.Summary, "$0.05 of your one-time $0.05 credit left") ||
		!strings.Contains(v.UploadsLine, "allowed now") || !strings.Contains(v.UploadsLine, "about 4472 days (~12 years)") {
		t.Fatalf("active view: %+v", v)
	}
	if strings.Contains(v.Summary+v.UploadsLine, "first upload is free") || strings.Contains(v.Summary+v.UploadsLine, "prepaid") {
		t.Fatalf("active trial must not call the upload free: %+v", v)
	}
	v = buildTrialView(trialAcct("top_up_required", "0", false, nil))
	if v.CanUpload || !strings.Contains(v.Summary, "used up") || !strings.HasPrefix(v.UploadsLine, "Uploads: blocked until you top up") {
		t.Fatalf("used view: %+v", v)
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
