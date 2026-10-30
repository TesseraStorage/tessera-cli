package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestAccountClosedMessage: moneyd answers 403 {"detail":{"error":"account_closed"}}
// for a deleted or terminated account on every authenticated route; each
// describer must say so plainly instead of echoing the raw error.
func TestAccountClosedMessage(t *testing.T) {
	body := []byte(`{"detail":{"error":"account_closed","message":"This account is closed (deleted or terminated); its credentials no longer work."}}`)
	err := parseBillingError(http.StatusForbidden, body)
	if err.Code != "account_closed" {
		t.Fatalf("Code = %q, want account_closed", err.Code)
	}
	for name, describe := range map[string]func(error) string{
		"billing": describeBillingError,
		"topup":   describeTopupError,
		"promo":   describePromoError,
	} {
		msg := describe(err)
		if msg != accountClosedMessage {
			t.Errorf("%s: %q, want the account-closed message", name, msg)
		}
		if !strings.Contains(msg, "account_closed") || !strings.Contains(msg, "tessera signup") {
			t.Errorf("%s: message lacks code or next step: %q", name, msg)
		}
	}
}

// TestForbiddenOtherCodeUnchanged: only account_closed gets the new text.
func TestForbiddenOtherCodeUnchanged(t *testing.T) {
	err := parseBillingError(http.StatusForbidden, []byte(`{"detail":{"error":"cross_origin_forbidden"}}`))
	if msg := describeBillingError(err); msg == accountClosedMessage {
		t.Fatalf("cross_origin_forbidden rendered as account closed")
	}
}
