package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZeroBalanceTechnicalValidation(t *testing.T) {
	in, known := zeroBalanceTechnicalValidation(trialAcct("free_trial", "0", true, nil))
	if !known || !in {
		t.Fatalf("free_trial $0: in=%v known=%v", in, known)
	}
	in, known = zeroBalanceTechnicalValidation(trialAcct("free_trial_active", "0.00", false, nil))
	if !known || !in {
		t.Fatalf("free_trial_active $0: in=%v known=%v", in, known)
	}

	// A balance, even with the free_trial flag still set, is not the allowance.
	in, known = zeroBalanceTechnicalValidation(trialAcct("free_trial", "5.00", true, nil))
	if !known || in {
		t.Fatalf("balance must not be technical validation: in=%v known=%v", in, known)
	}

	credited := trialAcct("free_trial_active", "0.25", true, float64(100))
	credited["readiness"].(map[string]any)["trial_credit_usd"] = "0.25"
	credited["readiness"].(map[string]any)["trial_credit_posted"] = true
	in, known = zeroBalanceTechnicalValidation(credited)
	if !known || in {
		t.Fatalf("posted credit must not be technical validation: in=%v known=%v", in, known)
	}

	funded := map[string]any{
		"balance_usd": "0",
		"readiness":   map[string]any{"state": "funded", "free_trial": false, "can_upload": false},
	}
	in, known = zeroBalanceTechnicalValidation(funded)
	if !known || in {
		t.Fatalf("funded $0 is not technical validation: in=%v known=%v", in, known)
	}

	in, known = zeroBalanceTechnicalValidation(map[string]any{"balance_usd": "0"})
	if known || in {
		t.Fatalf("missing readiness must be unknown: in=%v known=%v", in, known)
	}
	in, known = zeroBalanceTechnicalValidation(nil)
	if known || in {
		t.Fatalf("nil account must be unknown: in=%v known=%v", in, known)
	}
}

func TestTechnicalValidationOversizeMessage(t *testing.T) {
	got := technicalValidationOversizeMessage(50<<20, "")
	want := "Technical validation allows one upload of up to 40 MiB. This file is 50.00 MiB. Top up to upload larger files: tessera topup 5"
	if got != want {
		t.Fatalf("message =\n%q\nwant\n%q", got, want)
	}
	named := technicalValidationOversizeMessage(50<<20, "clips/a.mp4")
	if !strings.Contains(named, "50.00 MiB (clips/a.mp4)") || !strings.Contains(named, "tessera topup 5") {
		t.Fatalf("named message = %q", named)
	}
	low := strings.ToLower(got)
	for _, bad := range []string{"free trial", "foray", "72 hour", "subscription", "kept until"} {
		if strings.Contains(low, bad) {
			t.Fatalf("message contains %q", bad)
		}
	}
}

func TestRejectTechnicalValidationOversize(t *testing.T) {
	cfg := &Config{BillingAgentKey: "tsk_test", IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL}

	// Exactly one slab is allowed, and a small file must not consult the API.
	stubBillingClient(t, &fakeUsageAPI{err: errNoBillingCredentials})
	if err := rejectTechnicalValidationOversize(cfg, slabBytes); err != nil {
		t.Fatalf("exact slab refused: %v", err)
	}
	if err := rejectTechnicalValidationOversize(cfg, 1); err != nil {
		t.Fatalf("small file refused: %v", err)
	}

	// API failure: unknown, do not block.
	stubBillingClient(t, &fakeUsageAPI{err: errNoBillingCredentials})
	if err := rejectTechnicalValidationOversize(cfg, slabBytes+1); err != nil {
		t.Fatalf("unknown status refused: %v", err)
	}

	// No credential: unknown, do not block, even with no client.
	if err := rejectTechnicalValidationOversize(&Config{}, 50<<20); err != nil {
		t.Fatalf("no credential refused: %v", err)
	}

	stubBillingClient(t, &fakeUsageAPI{acct: trialAcct("free_trial", "0", true, nil)})
	err := rejectTechnicalValidationOversize(cfg, slabBytes+1)
	if err == nil {
		t.Fatal("oversize technical-validation file was allowed")
	}
	if err.Error() != technicalValidationOversizeMessage(slabBytes+1, "") {
		t.Fatalf("refusal = %q", err.Error())
	}
	if err := rejectTechnicalValidationOversize(cfg, 50<<20); err == nil || !strings.Contains(err.Error(), "50.00 MiB") {
		t.Fatalf("50 MiB refusal = %v", err)
	}

	stubBillingClient(t, &fakeUsageAPI{acct: trialAcct("funded", "12.50", true, float64(30))})
	if err := rejectTechnicalValidationOversize(cfg, 80<<20); err != nil {
		t.Fatalf("funded balance refused a large file: %v", err)
	}
	stubBillingClient(t, &fakeUsageAPI{acct: map[string]any{
		"balance_usd": "0",
		"readiness":   map[string]any{"state": "funded", "free_trial": false, "can_upload": true},
	}})
	if err := rejectTechnicalValidationOversize(cfg, 80<<20); err != nil {
		t.Fatalf("funded zero balance refused a large file: %v", err)
	}
}

func TestFirstOversizeUpload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"z.bin", filepath.Join("sub", "m.bin")} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(slabBytes + 1); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	// Ignored large file must not count.
	ignored, err := os.Create(filepath.Join(dir, "skip.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ignored.Truncate(80 << 20); err != nil {
		t.Fatal(err)
	}
	ignored.Close()
	if err := os.WriteFile(filepath.Join(dir, ".tesseraignore"), []byte("skip.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rel, size, found := firstOversizeUpload(dir)
	if !found || rel != "sub/m.bin" || size != slabBytes+1 {
		t.Fatalf("first oversize = %q %d found=%v", rel, size, found)
	}

	if err := os.WriteFile(filepath.Join(dir, ".tesseraignore"), []byte("skip.bin\nz.bin\nsub/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, found := firstOversizeUpload(dir); found {
		t.Fatal("ignored files were treated as uploads")
	}
}

func TestBatchTechnicalValidationGate(t *testing.T) {
	testConfigHome(t)
	large := map[string]localFile{"big.bin": {Size: slabBytes + 1}}
	ops := []Op{{Kind: OpUpload, Path: "big.bin"}}
	if batchBlocksTechnicalValidation(ops, large) {
		t.Fatal("no config must not block")
	}

	if err := saveConfig(&Config{
		IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL, BillingAgentKey: "tsk_test", AppID: "app",
	}); err != nil {
		t.Fatal(err)
	}
	stubBillingClient(t, &fakeUsageAPI{acct: trialAcct("free_trial", "0", true, nil)})
	if !batchBlocksTechnicalValidation(ops, large) {
		t.Fatal("technical validation must block the batch")
	}
	if batchBlocksTechnicalValidation(ops, map[string]localFile{"ok.bin": {Size: slabBytes}}) {
		t.Fatal("a one-slab file must not block")
	}
	stubBillingClient(t, &fakeUsageAPI{err: errNoBillingCredentials})
	if batchBlocksTechnicalValidation(ops, large) {
		t.Fatal("unreadable account must not block")
	}
}
