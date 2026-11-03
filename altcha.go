package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ALTCHA proof of work for signup (Money-10 stage 4). moneyd hands out an
// ALTCHA v1 challenge at GET /auth/altcha-challenge; the CLI finds the number
// n with sha256(salt + n) == challenge and sends the base64 JSON solution in
// the X-Altcha header of POST /agents and POST /auth/magic-link. Stdlib only:
// the protocol is a sha256 brute force, so no altcha-lib-go dependency.

const altchaHeader = "X-Altcha"

// altchaSolveTimeout bounds the search; the server's maxnumber (250000 by
// default) takes well under a second on any machine.
var altchaSolveTimeout = 60 * time.Second

type altchaChallenge struct {
	Algorithm string `json:"algorithm"`
	Challenge string `json:"challenge"`
	MaxNumber int    `json:"maxnumber"`
	Salt      string `json:"salt"`
	Signature string `json:"signature"`
}

// solveAltcha returns the base64 solution payload for ch.
func solveAltcha(ctx context.Context, ch altchaChallenge) (string, error) {
	if ch.Algorithm != "SHA-256" {
		return "", fmt.Errorf("unsupported proof-of-work algorithm %q", ch.Algorithm)
	}
	if ch.Challenge == "" || ch.Salt == "" || ch.Signature == "" || ch.MaxNumber <= 0 {
		return "", fmt.Errorf("malformed proof-of-work challenge")
	}
	want, err := hex.DecodeString(ch.Challenge)
	if err != nil || len(want) != sha256.Size {
		return "", fmt.Errorf("malformed proof-of-work challenge")
	}
	buf := make([]byte, 0, len(ch.Salt)+20)
	for n := 0; n <= ch.MaxNumber; n++ {
		if n%4096 == 0 && ctx.Err() != nil {
			return "", ctx.Err()
		}
		buf = strconv.AppendInt(append(buf[:0], ch.Salt...), int64(n), 10)
		sum := sha256.Sum256(buf)
		if string(sum[:]) == string(want) {
			payload, _ := json.Marshal(map[string]any{
				"algorithm": ch.Algorithm,
				"challenge": ch.Challenge,
				"number":    n,
				"salt":      ch.Salt,
				"signature": ch.Signature,
			})
			return base64.StdEncoding.EncodeToString(payload), nil
		}
	}
	return "", fmt.Errorf("proof-of-work challenge has no solution")
}

// altchaSolution fetches and solves a fresh challenge. Best-effort: any
// failure (an older server without the endpoint, a network error) yields ""
// and the request goes out without the header; the server decides.
func (b *httpBilling) altchaSolution() string {
	req, err := b.request(http.MethodGet, "/auth/altcha-challenge", nil)
	if err != nil {
		return ""
	}
	_, data, err := b.do(req, authNone, "")
	if err != nil {
		return ""
	}
	var ch altchaChallenge
	if json.Unmarshal(data, &ch) != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), altchaSolveTimeout)
	defer cancel()
	sol, err := solveAltcha(ctx, ch)
	if err != nil {
		return ""
	}
	return sol
}
