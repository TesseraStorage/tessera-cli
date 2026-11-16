package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The not-available-yet commands are absent from the main help and from
// the per-command help, and their message names the command.
func TestNotAvailableYetCommandsHiddenFromHelp(t *testing.T) {
	low := strings.ToLower(usageText)
	for _, bad := range []string{"{exe} share", "{exe} fetch", "{exe} sync", "{exe} folder", "{exe} service", "folders & sync", "anyone with the link", "renames are cheap"} {
		if strings.Contains(low, bad) {
			t.Errorf("usage text still mentions %q", bad)
		}
	}
	for _, h := range commandHelp {
		l := strings.ToLower(h)
		if strings.Contains(l, "tessera sync") || strings.Contains(l, "anyone with the link") || strings.Contains(l, "renames are cheap") {
			t.Errorf("command help still points at sync/share: %q", h)
		}
	}
	for _, c := range []string{"sync", "folder", "share", "fetch", "service"} {
		m := notAvailableYetMessage(c)
		if !strings.Contains(m, "not available yet") || !strings.Contains(m, " "+c+"'") {
			t.Errorf("message for %s: %q", c, m)
		}
	}
}

// End to end through the built binary: each retired command (with
// subcommands, aliases and --help) prints the message and exits non-zero
// without needing a login.
func TestNotAvailableYetCommandsExitNonZero(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "tessera")
	if out, err := exec.Command("go", "build", "-mod=vendor", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cases := [][]string{
		{"sync"}, {"sync", "add", ".", "--as", "x"}, {"sync", "watch"}, {"sync", "--help"},
		{"folder", "add"}, {"folder"}, {"share", "a.txt"}, {"share", "--help"},
		{"fetch", "https://example.com/x"}, {"fetch"},
		{"service", "install", "--yes"}, {"service", "start"},
	}
	for _, args := range cases {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "TESSERA_HOME="+filepath.Join(dir, "home"), "HOME="+dir)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 {
			t.Errorf("%v: want non-zero exit, got %v\n%s", args, err, out)
		}
		if !strings.Contains(string(out), "is not available yet") {
			t.Errorf("%v: missing message:\n%s", args, out)
		}
	}
}

func TestUploadBlockedErrorFromCanUpload(t *testing.T) {
	// Allowed or unknown: no refusal.
	for _, acct := range []map[string]any{
		{},
		{"readiness": map[string]any{"state": "funded"}},
		{"readiness": map[string]any{"state": "funded", "can_upload": true}},
		{"readiness": map[string]any{"state": "free_trial", "can_upload": true, "free_trial": true}},
	} {
		if err := uploadBlockedError(acct); err != nil {
			t.Errorf("%v: unexpected refusal %v", acct, err)
		}
	}
	// Technical validation used: top-up guidance.
	used := map[string]any{"balance_usd": "0", "readiness": map[string]any{
		"state": "free_trial_active", "can_upload": false, "top_up_required": true, "free_trial": true,
	}}
	err := uploadBlockedError(used)
	if _, ok := asPaymentRequired(err); !ok {
		t.Fatalf("want payment-required guidance, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "tessera topup 5") || !strings.Contains(msg, "Technical validation") || strings.Contains(strings.ToLower(msg), "free trial") {
		t.Errorf("guidance: %q", msg)
	}
	// No usable balance.
	zero := map[string]any{"balance_usd": "0", "readiness": map[string]any{
		"state": "top_up_required", "can_upload": false, "top_up_required": true,
	}}
	if err := uploadBlockedError(zero); err == nil || !strings.Contains(err.Error(), "tessera topup 5") {
		t.Errorf("zero balance: %v", err)
	}
	// Not a top-up case: the server's message, no top-up lines.
	paused := map[string]any{"readiness": map[string]any{
		"state": "paused", "can_upload": false, "message": "Account paused by Tessera. Contact info@tessera.storage.",
	}}
	err = uploadBlockedError(paused)
	if err == nil || !strings.Contains(err.Error(), "paused") || strings.Contains(err.Error(), "topup") {
		t.Errorf("paused: %v", err)
	}
}

func TestFolderUploadFailureCountsAndWrapsPayment(t *testing.T) {
	err := folderUploadFailure(1, 11, []uploadError{{RemotePath: "a", Err: errors.New("permission denied")}})
	if err == nil || !strings.Contains(err.Error(), "1 of 11") {
		t.Fatalf("got %v", err)
	}
	pe := &paymentRequiredError{Reason: "insufficient_balance"}
	err = folderUploadFailure(2, 3, []uploadError{{RemotePath: "a", Err: errors.New("x")}, {RemotePath: "b", Err: pe}})
	if _, ok := asPaymentRequired(err); !ok {
		t.Fatalf("payment refusal must stay visible: %v", err)
	}
}
