package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testChallenge(n int) altchaChallenge {
	salt := "abc123?expires=9999999999"
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s%d", salt, n)))
	return altchaChallenge{Algorithm: "SHA-256", Challenge: hex.EncodeToString(sum[:]), MaxNumber: 5000, Salt: salt, Signature: "sig"}
}

func TestSolveAltcha(t *testing.T) {
	sol, err := solveAltcha(context.Background(), testChallenge(4321))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(sol)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["number"].(float64) != 4321 || got["signature"] != "sig" || got["salt"] != "abc123?expires=9999999999" {
		t.Fatalf("solution = %v", got)
	}
}

func TestSolveAltchaRejectsBadChallenges(t *testing.T) {
	ch := testChallenge(10)
	ch.Algorithm = "SHA-1"
	if _, err := solveAltcha(context.Background(), ch); err == nil {
		t.Fatal("want error for SHA-1")
	}
	ch = testChallenge(6000) // above maxnumber
	if _, err := solveAltcha(context.Background(), ch); err == nil {
		t.Fatal("want no-solution error")
	}
}

func TestCreateAgentAndMagicLinkSendAltcha(t *testing.T) {
	ch := testChallenge(77)
	var agentsHeader, magicHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/altcha-challenge":
			_ = json.NewEncoder(w).Encode(ch)
		case "/agents":
			agentsHeader = r.Header.Get(altchaHeader)
			fmt.Fprint(w, `{"account_id":"AGNT-1","api_key":"k"}`)
		case "/auth/magic-link":
			magicHeader = r.Header.Get(altchaHeader)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"accepted"}`)
		}
	}))
	defer srv.Close()
	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: srv.URL}
	api := newBillingAPI(cfg)
	if _, _, err := api.CreateAgent("", ""); err != nil {
		t.Fatal(err)
	}
	if err := api.MagicLink("a@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{agentsHeader, magicHeader} {
		raw, err := base64.StdEncoding.DecodeString(h)
		if err != nil || !strings.Contains(string(raw), `"number":77`) {
			t.Fatalf("X-Altcha = %q (%s), want a solution with number 77", h, raw)
		}
	}
}

func TestSignupGuardErrorsShowServerMessage(t *testing.T) {
	for _, body := range []string{
		`{"error":"signup_limit","message":"Too many new accounts from your IP address today"}`,
		`{"error":"altcha_required","message":"Signup needs a proof-of-work solution. Update the Tessera CLI"}`,
	} {
		status := http.StatusTooManyRequests
		if strings.Contains(body, "altcha") {
			status = http.StatusBadRequest
		}
		err := error(parseBillingError(status, []byte(body)))
		msg := describeBillingError(err)
		var m map[string]string
		_ = json.Unmarshal([]byte(body), &m)
		if !strings.Contains(msg, m["message"]) {
			t.Fatalf("describe = %q, want server message", msg)
		}
	}
	nginx := parseBillingError(http.StatusTooManyRequests, []byte("<html><head><title>429 Too Many Requests</title></head></html>"))
	if msg := describeBillingError(nginx); !strings.Contains(msg, "too many requests") {
		t.Fatalf("nginx 429 describe = %q", msg)
	}
	_ = errors.New
}
