package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	indexdapp "go.sia.tech/indexd/api/appclient"
)

const sample402 = `{"error":"payment_required","reason":"trial_used","message":"Insufficient balance — top up to continue","detail":"Your free trial (one 40 MiB block upload and one download) has been used. Top up to continue.","topup_url":"https://tessera.storage/account","payment_options":[{"method":"wallet","cli":"tessera wallet init \u0026\u0026 tessera topup \u003cusd\u003e --wallet"`

func TestAsPaymentRequiredWrappedHTTPError(t *testing.T) {
	err := fmt.Errorf("failed to refresh hosts at write start: %w", fmt.Errorf("failed to fetch hosts from indexer: %w", &indexdapp.HTTPError{StatusCode: 402, Body: sample402}))
	pe, ok := asPaymentRequired(err)
	if !ok || pe.Reason != "trial_used" || pe.Message != "Insufficient balance — top up to continue" || !strings.Contains(pe.Detail, "free trial") {
		t.Fatalf("got %+v %v", pe, ok)
	}
	s := explainTransferError(nil, err, true).Error()
	for _, want := range []string{"Insufficient balance — top up to continue", "tessera topup 5", "--asset xmr", "tessera.storage/account"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %q", want, s)
		}
	}
	if strings.Contains(strings.ToLower(s), "no more hosts") {
		t.Fatal("402 must not read as a host shortage")
	}
}

func TestAsPaymentRequiredStringOnlyAndOthers(t *testing.T) {
	if pe, ok := asPaymentRequired(errors.New("download: HTTP 402: " + sample402)); !ok || pe.Reason != "trial_used" {
		t.Fatalf("string form not parsed: %+v", pe)
	}
	if _, ok := asPaymentRequired(&indexdapp.HTTPError{StatusCode: 500, Body: "x"}); ok {
		t.Fatal("500 is not a 402")
	}
	if _, ok := asPaymentRequired(errors.New("no more hosts available")); ok {
		t.Fatal("shortage is not a 402")
	}
	if got := explainFatalHostShortage("HTTP 402: " + sample402); !strings.Contains(got, "tessera topup 5") {
		t.Fatalf("fatal path: %q", got)
	}
}

func TestShortageAfterRecorded402(t *testing.T) {
	seenPaymentRequired.Store(nil)
	defer seenPaymentRequired.Store(nil)
	if _, ok := shortageAfterPaymentRequired(errors.New("download failed: no more hosts available")); ok {
		t.Fatal("no 402 seen yet")
	}
	resp := &http.Response{StatusCode: 402, Body: io.NopCloser(strings.NewReader(sample402))}
	resp, _ = notePaymentRequired(resp, nil)
	if b, _ := io.ReadAll(resp.Body); string(b) != sample402 {
		t.Fatal("body must be preserved")
	}
	msg := explainFatalHostShortage("download failed: no more hosts available")
	for _, want := range []string{"Insufficient balance — top up to continue", "Technical validation (one upload of up to 40 MiB and one download) has been used", "tessera topup 5", "tessera.storage/account"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %q", want, msg)
		}
	}
	if _, ok := shortageAfterPaymentRequired(errors.New("permission denied")); ok {
		t.Fatal("only shortages map to the 402")
	}
}

func TestBalanceRefusalMatchesServerWording(t *testing.T) {
	seenPaymentRequired.Store(nil)
	defer seenPaymentRequired.Store(nil)
	s := balanceRefusal(3, 10).Error()
	for _, want := range []string{"Insufficient balance — top up to continue", "only 3 of the 10 hosts", "tessera topup 5", "--asset xmr", "tessera.storage/account"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %q", want, s)
		}
	}
	notePaymentRequired(&http.Response{StatusCode: 402, Body: io.NopCloser(strings.NewReader(sample402))}, nil)
	if s := balanceRefusal(3, 10).Error(); !strings.Contains(s, "Technical validation") || strings.Contains(s, "free trial") || strings.Contains(s, "only 3 of the 10") {
		t.Fatalf("server detail not used: %q", s)
	}
}
