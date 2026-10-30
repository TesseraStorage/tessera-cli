package main

import (
	"context"
	"crypto/tls"
	"errors"
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

func init() {
	http.DefaultTransport = &retryTransport{next: http.DefaultTransport}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !retryableRequest(req) {
		return t.next.RoundTrip(req)
	}
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		resp, err = t.next.RoundTrip(req)
		if err == nil || attempt >= transientRetries || !isTransientNetErr(err) {
			return resp, err
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
	for _, frag := range []string{"401", "403", "unauthorized", "forbidden", "not registered", "unknown app", "invalid signature"} {
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
