package main

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	indexdapp "go.sia.tech/indexd/api/appclient"
)

// paymentRequiredError is the server's 402 "Insufficient balance — top up to
// continue" (host list, upload or download refused at $0 or after the free
// trial), turned into guidance with the commands that actually top up.
type paymentRequiredError struct {
	Reason  string
	Message string
	Detail  string
}

func (e *paymentRequiredError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "Insufficient balance — top up to continue"
	}
	var b strings.Builder
	b.WriteString(msg)
	if e.Detail != "" {
		b.WriteString("\n  " + e.Detail)
	}
	b.WriteString("\n  Pay from this CLI's wallet (USDC on Base): tessera wallet init && tessera topup 5 --wallet")
	b.WriteString("\n  Pay in XMR, BTC, ETH, USDT or another coin:  tessera topup 5 --asset xmr   (minimums: tessera topup --minimums)")
	b.WriteString("\n  Or top up at https://tessera.storage/account")
	return b.String()
}

var paymentFieldRE = map[string]*regexp.Regexp{
	"reason":  regexp.MustCompile(`"reason"\s*:\s*"((?:[^"\\]|\\.)*)"`),
	"message": regexp.MustCompile(`"message"\s*:\s*"((?:[^"\\]|\\.)*)"`),
	"detail":  regexp.MustCompile(`"detail"\s*:\s*"((?:[^"\\]|\\.)*)"`),
}

// paymentField pulls one string field out of a possibly truncated JSON body
// (the SDK keeps only the first 1 KiB of an error body).
func paymentField(body, name string) string {
	m := paymentFieldRE[name].FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	if s, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
		return s
	}
	return m[1]
}

// asPaymentRequired reports whether err is an HTTP 402 from the indexer or
// host-filter, returning the parsed refusal.
func asPaymentRequired(err error) (*paymentRequiredError, bool) {
	if err == nil {
		return nil, false
	}
	var pe *paymentRequiredError
	if errors.As(err, &pe) {
		return pe, true
	}
	var body string
	var he *indexdapp.HTTPError
	if errors.As(err, &he) {
		if he.StatusCode != http.StatusPaymentRequired {
			return nil, false
		}
		body = he.Body
	} else {
		s := err.Error()
		i := strings.Index(s, "HTTP 402")
		if i < 0 {
			return nil, false
		}
		body = s[i:]
	}
	return &paymentRequiredError{
		Reason:  paymentField(body, "reason"),
		Message: paymentField(body, "message"),
		Detail:  paymentField(body, "detail"),
	}, true
}

// explainPaymentRequired returns the top-up guidance for a 402, or err as is.
func explainPaymentRequired(err error) error {
	if pe, ok := asPaymentRequired(err); ok {
		return fmt.Errorf("%w", pe)
	}
	return err
}
