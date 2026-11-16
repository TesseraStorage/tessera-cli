package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.sia.tech/coreutils/wallet"
)

// N1: a supplied phrase is checked on this machine before any server call.
func TestCheckPhraseFormat(t *testing.T) {
	if err := checkPhraseFormat(wallet.NewSeedPhrase()); err != nil {
		t.Fatalf("valid phrase refused: %v", err)
	}
	err := checkPhraseFormat("one two three")
	if !errors.Is(err, errPhraseFormat) || !strings.Contains(err.Error(), "12 words (got 3)") {
		t.Fatalf("3 words: %v", err)
	}
	err = checkPhraseFormat("abandon ability able about above absent absorb abstract absurd abuse access accident")
	if !errors.Is(err, errPhraseFormat) || !strings.Contains(err.Error(), "not a valid recovery phrase") {
		t.Fatalf("bad checksum: %v", err)
	}
}

// N1: an --api-key login refused for want of the phrase leaves no
// half-login (API key saved with a blank App ID) behind.
func TestAPIKeyLoginWithoutPhraseSavesNothing(t *testing.T) {
	home := testConfigHome(t)
	fake := &fakeLoginAPI{
		recoveryAppID:  strings.Repeat("ab", 32),
		recoveryPublic: "ed25519:" + strings.Repeat("cd", 32),
		recoveryKey:    "ck_existing",
		account:        map[string]any{"billing_id": "acct-linked"},
	}
	stubHeadlessRegistration(t, fake)
	t.Setenv("TESSERA_PHRASE", "")
	err := runLogin([]string{"--api-key", "tsk_deadbeef_examplekeyvalue________________"})
	if !errors.Is(err, errRecoveryPhraseRequired) {
		t.Fatalf("runLogin = %v, want errRecoveryPhraseRequired", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json was left behind (stat err %v)", err)
	}
}

// N1: in recovery mode the indexer's "no remaining uses" means the phrase
// derived a different key.
func TestConnectKeyUsedUpMeansMismatch(t *testing.T) {
	if !isConnectKeyUsedUp(errors.New("failed to register app key: HTTP 403: key has no remaining uses")) {
		t.Fatal("not recognised")
	}
	if isConnectKeyUsedUp(errors.New("HTTP 500")) {
		t.Fatal("false positive")
	}
}

func TestIsInternalPath(t *testing.T) {
	for p, want := range map[string]bool{
		".tessera-trash/2d27fbdf/vt/ver.txt": true,
		"a/.tessera-versions/x/1":            true,
		".tessera-versions":                  true,
		"vt/ver.txt":                         false,
		"my.tessera-trash.txt":               false,
	} {
		if got := isInternalPath(p); got != want {
			t.Errorf("isInternalPath(%q) = %v", p, got)
		}
	}
	idx := &Index{Paths: map[string]*IndexEntry{}}
	idx.put(PathInfo{Path: ".tessera-trash/ab/a.txt", Name: "a.txt"})
	if _, err := idx.lookup("a.txt"); !errors.Is(err, errNotFound) {
		t.Fatalf("a bare name resolved to a trash copy: %v", err)
	}
}

func TestEarlierCopies(t *testing.T) {
	all := []PathInfo{
		{ObjectID: "old", Path: "vt/ver.txt", Updated: time.Unix(1, 0)},
		{ObjectID: "cur", Path: "vt/ver.txt", Updated: time.Unix(2, 0)},
		{ObjectID: "other", Path: "x.txt"},
	}
	got := earlierCopies(all, &IndexEntry{ObjectID: "cur", Path: "vt/ver.txt"})
	if len(got) != 1 || got[0].ObjectID != "old" {
		t.Fatalf("earlierCopies = %#v", got)
	}
}

func TestRejectUnknownFlags(t *testing.T) {
	if err := rejectUnknownFlags("upload", []string{"a.txt", "--as", "--x", "--json"}, "--as", "--json"); err != nil {
		t.Fatalf("--as value treated as a flag: %v", err)
	}
	err := rejectUnknownFlags("upload", []string{"a.txt", "--bogus"}, "--as")
	if err == nil || !strings.Contains(err.Error(), "unknown option --bogus") {
		t.Fatalf("got %v", err)
	}
}

func TestCheckRemotePath(t *testing.T) {
	for _, bad := range []string{"/abs", "../../etc/x", ".tessera-trash/x", ""} {
		if _, err := checkRemotePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := checkRemotePath("docs//report.pdf"); err != nil || got != "docs/report.pdf" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestExplainOffline(t *testing.T) {
	raw := errors.New(`failed to check app auth: failed to check app auth: Get "https://index.tessera.storage/auth/check?x=1": proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused`)
	got := explainOffline(raw).Error()
	if !strings.Contains(got, "cannot reach index.tessera.storage: the connection was refused") || strings.Contains(got, "check app auth") {
		t.Fatalf("got %q", got)
	}
	plain := errors.New("HTTP 400: bad request")
	if explainOffline(plain) != plain {
		t.Fatal("a non-network error was rewritten")
	}
}

func TestRemoveStaleTemps(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, tempFilePrefix+"1")
	fresh := filepath.Join(dir, tempFilePrefix+"2")
	keep := filepath.Join(dir, "file.txt")
	for _, p := range []string{old, fresh, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)
	_ = os.Chtimes(keep, past, past)
	if n := removeStaleTemps(dir); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for p, want := range map[string]bool{old: false, fresh: true, keep: true} {
		_, err := os.Stat(p)
		if (err == nil) != want {
			t.Errorf("%s exists=%v want %v", p, err == nil, want)
		}
	}
}

func TestCheckDownloadDest(t *testing.T) {
	dir := t.TempDir()
	if err := checkDownloadDest(dir); err == nil || !strings.Contains(err.Error(), "is a folder") {
		t.Fatalf("folder accepted: %v", err)
	}
	if err := checkDownloadDest(filepath.Join(dir, "new", "sub", "f.bin")); err != nil {
		t.Fatalf("creatable path refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Fatal("the check created folders")
	}
}

// N10: long exact balances print with at most six decimals.
func TestForfeitedBalanceSixDecimals(t *testing.T) {
	if got := signedMoney("0.0087137467733333333320"); got != "$0.008714" {
		t.Fatalf("got %s", got)
	}
}

// N13: a refused app key on an account that was deleted elsewhere says so.
func TestExplainAuthFailureAccountClosed(t *testing.T) {
	_ = testConfigHome(t)
	stubBillingClient(t, &fakeLoginAPI{accountErr: &billingError{Status: http.StatusForbidden, Code: "account_closed"}})
	cfg := &Config{BillingAgentKey: "tsk_x"}
	if err := explainAuthFailure(cfg, errors.New("HTTP 401: unknown account")); !errors.Is(err, errMachineAccountClosed) {
		t.Fatalf("got %v", err)
	}
}
