package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sia.tech/core/types"
)

const seedTestPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func TestDeriveRecoveryKeyNormalizesPhrase(t *testing.T) {
	a := recoveryPublicHex(deriveRecoveryKey(seedTestPhrase))
	b := recoveryPublicHex(deriveRecoveryKey("  ABANDON abandon\tabandon abandon abandon abandon abandon abandon abandon abandon abandon   About \n"))
	if a != b {
		t.Fatalf("normalization differs: %s vs %s", a, b)
	}
	if c := recoveryPublicHex(deriveRecoveryKey(strings.Replace(seedTestPhrase, "about", "able", 1))); c == a {
		t.Fatal("different phrases derived the same key")
	}
	if len(a) != 64 {
		t.Fatalf("public key hex length %d", len(a))
	}
}

// TestSeedLoginRestoresSameAccountHeadless: with NO config at all,
// `tessera login "<phrase>"` proves phrase possession to moneyd, receives the
// account's API key, and restores the account's existing App ID without any
// browser or app password.
func TestSeedLoginRestoresSameAccountHeadless(t *testing.T) {
	testConfigHome(t)
	t.Setenv("TESSERA_PHRASE", "")
	wantPK := recoveryPublicHex(deriveRecoveryKey(seedTestPhrase))
	const appID = "e2ba33c7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/recover":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("recover must be unauthenticated")
			}
			var body struct {
				PK  string `json:"recovery_public_key"`
				TS  int64  `json:"timestamp"`
				Sig string `json:"signature"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			pk, _ := hex.DecodeString(body.PK)
			sig, _ := hex.DecodeString(body.Sig)
			msg := fmt.Sprintf("tessera-recover:v1:%s:%d", body.PK, body.TS)
			if body.PK != wantPK || len(pk) != ed25519.PublicKeySize || !ed25519.Verify(pk, []byte(msg), sig) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"detail":{"error":"recovery_failed"}}`)
				return
			}
			fmt.Fprint(w, `{"account_id":"AGNT-1","kind":"agent","api_key":"tsk_recovered"}`)
		case "/credentials/recovery":
			if r.Header.Get("Authorization") != "Bearer tsk_recovered" {
				t.Errorf("recovery auth = %q", r.Header.Get("Authorization"))
			}
			fmt.Fprintf(w, `{"app_id":%q,"connect_key":"ck_existing"}`, appID)
		case "/credentials/activate":
			fmt.Fprint(w, `{"linked":true}`)
		case "/account":
			fmt.Fprint(w, `{"billing_id":"AGNT-1","kind":"agent","status":"active","balance_usd":"0","readiness":{"state":"free_trial","message":"Free trial: your first upload is prepaid."}}`)
		case "/credentials/recovery-key":
			fmt.Fprint(w, `{"registered":true,"changed":false}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL)

	builder := &fakeRegistrationBuilder{
		respURL: "http://index.example/auth/connect/req-1",
		app:     &fakeRegisteredSDK{key: types.GeneratePrivateKey()},
	}
	stubSDKBuilder(t, builder)
	var rec approveRecorder
	stubApproveConnectURL(t, &rec, nil)
	origBrowser := openBrowser
	openBrowser = func(string) error { t.Error("browser must not open"); return nil }
	t.Cleanup(func() { openBrowser = origBrowser })

	var err error
	out := captureStdout(t, func() { err = runLogin([]string{seedTestPhrase}) })
	if err != nil {
		t.Fatalf("runLogin: %v\n%s", err, out)
	}
	if paths[0] != "/auth/recover" {
		t.Fatalf("first request %v, want /auth/recover", paths)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.BillingAgentKey != "tsk_recovered" || saved.AppID != appID || saved.AppKey == "" {
		t.Fatalf("restored config wrong: agent %q app %q", saved.BillingAgentKey, saved.AppID)
	}
	if builder.phrase != seedTestPhrase {
		t.Fatalf("registered with phrase %q", builder.phrase)
	}
	for _, bad := range []string{"app password", "App password", "browser"} {
		if strings.Contains(out, bad) {
			t.Errorf("output mentions %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "AGNT-1") || !strings.Contains(out, "Free trial") {
		t.Errorf("output lacks account / readiness:\n%s", out)
	}
}

func TestSeedLoginNotRegisteredGivesGuidance(t *testing.T) {
	testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":{"error":"recovery_key_not_registered"}}`)
	}))
	defer srv.Close()
	t.Setenv("TESSERA_BILLING_URL", srv.URL)
	err := runLogin([]string{seedTestPhrase})
	if err == nil || !strings.Contains(err.Error(), "--api-key") {
		t.Fatalf("err = %v, want --api-key guidance", err)
	}
}

func TestSeedLoginRejectsShortPhrase(t *testing.T) {
	testConfigHome(t)
	if err := runLogin([]string{"one", "two"}); err == nil || !strings.Contains(err.Error(), "12 words") {
		t.Fatalf("err = %v", err)
	}
}

func TestExpectedSHA256(t *testing.T) {
	sums := []byte("abc123  tessera-linux-amd64\ndef456 *tessera-windows-amd64.exe\n")
	if h, err := expectedSHA256(sums, "tessera-windows-amd64.exe"); err != nil || h != "def456" {
		t.Fatalf("%q %v", h, err)
	}
	if _, err := expectedSHA256(sums, "tessera-darwin-arm64"); err == nil {
		t.Fatal("missing entry accepted")
	}
	if releaseAssetName("windows", "arm64") != "tessera-windows-arm64.exe" || releaseAssetName("linux", "amd64") != "tessera-linux-amd64" {
		t.Fatal("asset names")
	}
}

// TestUpdateRefusesChecksumMismatch: a tampered binary must never replace
// the executable. Runs runUpdate against a fake release server, with
// os.Executable pointing at the test binary — so only the refusal path is
// exercised (success would overwrite the test binary).
func TestUpdateRefusesChecksumMismatch(t *testing.T) {
	testConfigHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), releaseAssetName(runtimeGOOS(), runtimeGOARCH()))
		default:
			fmt.Fprint(w, "not the real binary")
		}
	}))
	defer srv.Close()
	t.Setenv("TESSERA_UPDATE_BASE", srv.URL)
	exe, _ := os.Executable()
	before, _ := os.Stat(exe)
	var err error
	captureStdout(t, func() { err = runUpdate([]string{"--version", "v99.0.0"}) })
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	after, _ := os.Stat(exe)
	if before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		t.Fatal("executable changed despite checksum mismatch")
	}
}
