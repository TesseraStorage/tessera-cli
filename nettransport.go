package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Transient network failures (a dropped keep-alive answered with a bare EOF,
// a reset connection, a TLS handshake cut short) are common on sandboxed and
// cloud networks. Every HTTP client in the CLI, including the vendored
// indexer client, uses http.DefaultTransport, so wrapping it once makes all
// idempotent requests (GET/HEAD without a body) retry a few times before an
// error reaches the user. Requests with a body are never retried here.

const (
	transientRetries   = 3
	transientRetryBase = 400 * time.Millisecond
)

type retryTransport struct {
	next http.RoundTripper
}

// connectTimeout bounds how long one attempt to open a connection (TCP and
// TLS) may take. Go's default is 30 s, which made a blackholed network look
// like a hang; a reachable server answers well inside 10 s.
const connectTimeout = 10 * time.Second

func init() {
	next := http.DefaultTransport
	if t, ok := next.(*http.Transport); ok {
		t = t.Clone()
		t.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
		t.TLSHandshakeTimeout = connectTimeout
		next = t
	}
	http.DefaultTransport = &retryTransport{next: next}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		// Identify the CLI version on every request (optional for the
		// service; it only shows which versions are still in use).
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", userAgent())
	}
	resp, err := t.roundTrip(req)
	noteLatestHeader(req, resp)
	return resp, err
}

func (t *retryTransport) roundTrip(req *http.Request) (*http.Response, error) {
	if !retryableRequest(req) {
		return notePaymentRequired(t.next.RoundTrip(req))
	}
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		resp, err = t.next.RoundTrip(req)
		if err == nil || attempt >= transientRetries || !isTransientNetErr(err) {
			return notePaymentRequired(resp, err)
		}
		select {
		case <-req.Context().Done():
			return nil, err
		case <-time.After(transientRetryBase * time.Duration(1<<attempt)):
		}
	}
}

func retryableRequest(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return req.Body == nil || req.Body == http.NoBody
}

// isTransientNetErr reports errors worth retrying: the connection broke, not
// the server saying no. Timeouts of the caller's own context are excluded.
func isTransientNetErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	// A connection attempt that timed out is not retried: the network is
	// down or blocked, and every retry would wait the full connect timeout.
	var operr *net.OpError
	if errors.As(err, &operr) && (operr.Op == "dial" || operr.Op == "proxyconnect") && operr.Timeout() {
		return false
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	s := err.Error()
	if strings.Contains(s, "x509:") || strings.Contains(s, "certificate") {
		return false // a bad certificate does not fix itself
	}
	for _, frag := range []string{"EOF", "connection reset", "broken pipe", "TLS handshake timeout", "server closed idle connection", "no such host"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// signedURLQuery matches the query string of an http(s) URL. Indexer and
// share URLs carry signatures and keys in the query; errors must not leak them.
var signedURLQuery = regexp.MustCompile(`(https?://[^\s"'?#]+)\?[^\s"'#]*`)

// redactURLs replaces every URL query string in s with "?<redacted>".
func redactURLs(s string) string {
	return signedURLQuery.ReplaceAllString(s, "$1?<redacted>")
}

// looksLikeAuthError reports whether err means the credentials were refused,
// as opposed to a network failure.
func looksLikeAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{"401", "403", "unauthorized", "forbidden", "not registered", "unknown app", "invalid signature", "not authorized", "unknown account"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// connectionHint is the advice appended to a failed indexer connection.
func connectionHint(err error) string {
	if looksLikeAuthError(err) {
		return "try 'tessera login'"
	}
	return "check the network and retry (the login is fine; 'tessera status' shows readiness)"
}

// requestHost matches the host of the URL in a Go HTTP client error
// ("Get \"https://host/path...\": ...").
var requestHost = regexp.MustCompile(`"https?://([^/"?:]+)`)

// offlineReason names a connection failure in plain words, or "" when err is
// not one (an HTTP status, a refused key, bad data).
func offlineReason(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no such host") || strings.Contains(s, "server misbehaving") || strings.Contains(s, "temporary failure in name resolution"):
		return "its name could not be looked up (DNS)"
	case strings.Contains(s, "connection refused"):
		return "the connection was refused"
	case strings.Contains(s, "network is unreachable") || strings.Contains(s, "no route to host"):
		return "the network is unreachable"
	case strings.Contains(s, "i/o timeout") || strings.Contains(s, "tls handshake timeout") ||
		(strings.Contains(s, "dial tcp") && strings.Contains(s, "timeout")) ||
		strings.Contains(s, "client.timeout exceeded") || strings.Contains(s, "context deadline exceeded") && strings.Contains(s, "dial"):
		return "the connection timed out"
	case strings.Contains(s, "connection reset") && strings.Contains(s, "dial"):
		return "the connection was reset"
	}
	return ""
}

// explainOffline replaces a raw connection error with one plain sentence that
// names the server and what went wrong. Other errors are returned unchanged.
func explainOffline(err error) error {
	reason := offlineReason(err)
	if reason == "" {
		return err
	}
	host := "the Tessera servers"
	if m := requestHost.FindStringSubmatch(err.Error()); m != nil {
		host = m[1]
	}
	return &offlineError{msg: fmt.Sprintf("cannot reach %s: %s. Check this machine's internet connection (and proxy settings, if any), then try again; nothing was changed", host, reason), err: err}
}

type offlineError struct {
	msg string
	err error
}

func (e *offlineError) Error() string { return e.msg }
func (e *offlineError) Unwrap() error { return e.err }
