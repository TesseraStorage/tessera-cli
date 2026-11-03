package main

import (
	"errors"
	"fmt"
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
	for _, want := range []string{"Insufficient balance — top up to continue", "tessera topup 5 --wallet", "--asset xmr", "tessera.storage/account"} {
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
