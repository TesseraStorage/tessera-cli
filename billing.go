package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// sessionCookieName is the moneyd session cookie captured by Verify.
	sessionCookieName = "tessera_session"
	// billingHTTPTimeout bounds one billing API call.
	billingHTTPTimeout = 30 * time.Second
	// billingErrorBodyLimit caps how much of a response body is read/kept.
	billingErrorBodyLimit = 1 << 20
)

// errNoBillingCredentials means a protected call was attempted before any
// billing credential existed.
var errNoBillingCredentials = errors.New("no billing credentials — run 'tessera signup' or 'tessera login'")

// errSessionExpired is returned when the server rejects the saved
// tessera_session cookie. Callers print its message; the client has already
// cleared the dead session from the config.
var errSessionExpired = errors.New("session expired — run `tessera login --email …`")

// billingError is a typed error from the moneyd billing API. moneyd returns
// most errors nested ({"detail":{"error":"code", …}}) and a few flat
// ({"error":"code", …}); Code is parsed from either shape and Extra preserves
// the sibling fields callers need (minimum_usd, transaction, accepts, …).
type billingError struct {
	Status int
	Code   string
	Extra  map[string]any
}

func (e *billingError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("billing API error (HTTP %d)", e.Status)
	}
	return fmt.Sprintf("billing API error (HTTP %d): %s", e.Status, e.Code)
}

// describeBillingError renders a billing error for the command line. A 401 on
// a bearer-authenticated call is a missing/rejected/revoked agent key, not the
// session-expiry signal (a cookie 401 becomes errSessionExpired in do), so it
// gets the shared sign-in guidance every billing command prints: sign up or
// log in again, and a revoked agent key can only be replaced by a new agent
// account via `signup --force`. Anything else passes through as the client
// rendered it.
// accountClosedMessage is shown for 403 account_closed: the account was
// deleted or terminated, every key and session for it is dead, and top-ups,
// promos and credentials are refused. Only a new account helps.
const accountClosedMessage = "account_closed — this account has been deleted or closed, so its API key and sign-in no longer work and it cannot be topped up; create a new account with 'tessera signup --force'"

func describeBillingError(err error) string {
	var be *billingError
	if errors.As(err, &be) && be.Status == http.StatusForbidden && be.Code == "account_closed" {
		return accountClosedMessage
	}
	if errors.As(err, &be) && be.Status == http.StatusUnauthorized {
		return fmt.Sprintf("authentication failed — run 'tessera signup' or 'tessera login' to sign in; if an agent key was revoked, run 'tessera signup --force' for a new agent account (%s)", be)
	}
	return err.Error()
}

// parseBillingError decodes either moneyd error shape. Siblings of the error
// code are collected into Extra: nested detail fields and top-level fields
// (other than error/detail) alike.
func parseBillingError(status int, body []byte) *billingError {
	e := &billingError{Status: status, Extra: map[string]any{}}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		e.Code = truncate(strings.TrimSpace(string(body)), 200)
		return e
	}
	if detail, ok := raw["detail"].(map[string]any); ok {
		if code, ok := detail["error"].(string); ok {
			e.Code = code
		}
		for k, v := range detail {
			if k != "error" {
				e.Extra[k] = v
			}
		}
	} else if detail, ok := raw["detail"].(string); ok {
		e.Code = detail
	}
	if e.Code == "" {
		if code, ok := raw["error"].(string); ok {
			e.Code = code
		}
	}
	for k, v := range raw {
		if k != "error" && k != "detail" {
			e.Extra[k] = v
		}
	}
	return e
}

// billingAPI is the moneyd surface the CLI commands use. It is an interface so
// command tests can substitute an in-memory fake; the real implementation is
// httpBilling.
type billingAPI interface {
	MagicLink(email string) error
	Verify(token string) (session string, err error)
	Logout(session string) error
	CreateAgent(name, contactEmail string) (accountID, apiKey string, err error)
	Account() (map[string]any, error)
	Credentials() (connectKey string, err error)
	RecoveryCredential() (appID, publicKey, connectKey string, err error)
	Activate() error
	Topup(amount, rail, payCurrency, paymentHeader string) (resp map[string]any, err error)
	Promo(code string) (map[string]any, error)
}

// Optional billing capabilities. They are separate interfaces so the existing
// fakes and any older client keep satisfying billingAPI; callers type-assert
// and fall back to the original behaviour when a capability is absent.
type topupModeAPI interface {
	TopupWithMode(amount, rail, payCurrency, paymentHeader, mode string) (map[string]any, error)
}

type topupStatusAPI interface {
	TopupStatus(paymentIntentID string) (map[string]any, error)
}

type currencyAPI interface {
	Currencies() ([]map[string]any, error)
}

type x402ChallengeAPI interface {
	X402Challenge(amount string) (map[string]any, error)
}

// httpBilling speaks moneyd's public /api/v1 surface over net/http.
type httpBilling struct {
	base     string
	client   *http.Client
	session  string
	agentKey string
}

var _ billingAPI = (*httpBilling)(nil)

// newBillingAPI builds the moneyd client for cfg. The TESSERA_BILLING_URL
// environment variable overrides the configured base for development and
// tests. Protected calls authenticate with the saved session cookie when one is
// set, otherwise with the agent key as a bearer token: a stored session (an
// explicit human login) wins over an agent key, and a 401 clears the session
// so later calls fall back to the key.
func newBillingAPI(cfg *Config) *httpBilling {
	base := defaultBillingURL
	if cfg != nil && cfg.BillingAPIURL != "" {
		base = cfg.BillingAPIURL
	}
	if override := os.Getenv("TESSERA_BILLING_URL"); override != "" {
		base = override
	}
	b := &httpBilling{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: billingHTTPTimeout},
	}
	if cfg != nil {
		b.session = cfg.BillingSession
		b.agentKey = cfg.BillingAgentKey
	}
	return b
}

// request builds a JSON request against the configured base.
func (b *httpBilling) request(method, path string, body any) (*http.Request, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, strings.TrimRight(b.base, "/")+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// authMode selects the credentials attached to a request.
type authMode int

const (
	authNone   authMode = iota // public endpoint
	authAuto                   // session cookie when set, else the bearer key
	authCookie                 // the session cookie (an explicit override allowed)
)

// billingOrigin returns the "scheme://host[:port]" origin of the billing base.
// moneyd's CSRF guard rejects an unsafe request that carries the
// tessera_session cookie unless it presents an Origin (or Referer) matching
// TESSERA_PUBLIC_BASE_URL. A CLI is not a browser, but it holds the same
// session cookie, so it must prove the same origin a same-site browser would.
func billingOrigin(base string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// do sends req and reads the (bounded) response body. A 401 on a
// cookie-authenticated request is the session-expiry signal: the dead session
// is dropped in this client and in the config, and errSessionExpired is
// returned so callers can print the login guidance. A later authAuto call then
// falls back to an agent key when one is stored. Bearer callers get the plain
// billingError.
func (b *httpBilling) do(req *http.Request, mode authMode, sessionOverride string) (*http.Response, []byte, error) {
	cookieUsed := false
	switch mode {
	case authAuto:
		if b.session != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: b.session})
			cookieUsed = true
		} else if b.agentKey != "" {
			req.Header.Set("Authorization", "Bearer "+b.agentKey)
		} else {
			return nil, nil, errNoBillingCredentials
		}
	case authCookie:
		session := b.session
		if sessionOverride != "" {
			session = sessionOverride
		}
		if session == "" {
			return nil, nil, errNoBillingCredentials
		}
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		cookieUsed = true
	}
	if cookieUsed {
		// Satisfy moneyd's session-cookie CSRF guard; see billingOrigin.
		if origin := billingOrigin(b.base); origin != "" {
			req.Header.Set("Origin", origin)
		}
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("billing API request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, billingErrorBodyLimit))
	if err != nil {
		return nil, nil, fmt.Errorf("read billing API response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if cookieUsed && resp.StatusCode == http.StatusUnauthorized {
			// Drop the dead session here as well as in the config so later
			// calls on this client use a fallback credential (an agent key)
			// instead of replaying the rejected cookie.
			b.session = ""
			clearBillingSession()
			return resp, data, errSessionExpired
		}
		return resp, data, parseBillingError(resp.StatusCode, data)
	}
	return resp, data, nil
}

// clearBillingSession drops the saved session from the config after the server
// rejected it, so later commands fail fast with the login guidance instead of
// replaying a dead cookie. Best-effort: a config failure must not replace
// errSessionExpired, which the caller is already surfacing.
func clearBillingSession() {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	if cfg.BillingSession == "" {
		return
	}
	cfg.BillingSession = ""
	_ = saveConfig(cfg)
}

// decodeBillingObject parses a JSON object body; an empty body yields an empty
// object (some success responses are `{}` or have no body at all).
func decodeBillingObject(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode billing API response: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("decode billing API response: not a JSON object")
	}
	return out, nil
}

// MagicLink asks moneyd to email a single-use sign-in link. Public endpoint.
func (b *httpBilling) MagicLink(email string) error {
	req, err := b.request(http.MethodPost, "/auth/magic-link", map[string]any{"email": email})
	if err != nil {
		return err
	}
	_, _, err = b.do(req, authNone, "")
	return err
}

// Verify exchanges a magic-link token for a session and returns the
// tessera_session cookie value. Public endpoint.
func (b *httpBilling) Verify(token string) (string, error) {
	req, err := b.request(http.MethodPost, "/auth/verify", map[string]any{"token": token})
	if err != nil {
		return "", err
	}
	resp, _, err := b.do(req, authNone, "")
	if err != nil {
		return "", err
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c.Value, nil
		}
	}
	return "", fmt.Errorf("verify succeeded but no %s cookie was returned", sessionCookieName)
}

// Logout revokes session server-side. The explicit session wins over the one
// built into the client; a bearer is never used here.
func (b *httpBilling) Logout(session string) error {
	req, err := b.request(http.MethodPost, "/auth/logout", nil)
	if err != nil {
		return err
	}
	_, _, err = b.do(req, authCookie, session)
	return err
}

// CreateAgent opens a pending agent account and returns the account id and the
// one-time API key. Public endpoint.
func (b *httpBilling) CreateAgent(name, contactEmail string) (string, string, error) {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if contactEmail != "" {
		body["contact_email"] = contactEmail
	}
	req, err := b.request(http.MethodPost, "/agents", body)
	if err != nil {
		return "", "", err
	}
	_, data, err := b.do(req, authNone, "")
	if err != nil {
		return "", "", err
	}
	obj, err := decodeBillingObject(data)
	if err != nil {
		return "", "", err
	}
	accountID, _ := obj["account_id"].(string)
	apiKey, _ := obj["api_key"].(string)
	if accountID == "" || apiKey == "" {
		return "", "", fmt.Errorf("agents response is missing account_id or api_key")
	}
	return accountID, apiKey, nil
}

// Account fetches the authenticated account dashboard summary.
func (b *httpBilling) Account() (map[string]any, error) {
	req, err := b.request(http.MethodGet, "/account", nil)
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}

// Credentials mints the account's one connect key (409 identity_exists when
// the account already has an active identity).
func (b *httpBilling) Credentials() (string, error) {
	req, err := b.request(http.MethodPost, "/credentials", map[string]any{})
	if err != nil {
		return "", err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return "", err
	}
	obj, err := decodeBillingObject(data)
	if err != nil {
		return "", err
	}
	connectKey, _ := obj["connect_key"].(string)
	if connectKey == "" {
		return "", fmt.Errorf("credentials response is missing connect_key")
	}
	return connectKey, nil
}

// RecoveryCredential returns the material for restoring the account's
// existing storage identity from its recovery phrase: the original app id,
// the account public key (so a wrong phrase can be rejected before anything
// is persisted) and the raw connect key used as the one-time approval
// password. 404 identity_not_found means the account has no active storage
// identity yet; the caller mints one with Credentials instead.
func (b *httpBilling) RecoveryCredential() (appID, publicKey, connectKey string, err error) {
	req, err := b.request(http.MethodPost, "/credentials/recovery", map[string]any{})
	if err != nil {
		return "", "", "", err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return "", "", "", err
	}
	obj, err := decodeBillingObject(data)
	if err != nil {
		return "", "", "", err
	}
	appID, _ = obj["app_id"].(string)
	publicKey, _ = obj["public_key"].(string)
	connectKey, _ = obj["connect_key"].(string)
	if connectKey == "" {
		return "", "", "", fmt.Errorf("recovery response is missing connect_key")
	}
	return appID, publicKey, connectKey, nil
}

// Activate runs the storage-identity link synchronously, closing the
// fail-closed window right after registration.
func (b *httpBilling) Activate() error {
	req, err := b.request(http.MethodPost, "/credentials/activate", nil)
	if err != nil {
		return err
	}
	_, _, err = b.do(req, authAuto, "")
	return err
}

// Topup opens a NOWPayments invoice or, with rail "x402", drives the x402
// challenge/settle flow. A non-empty paymentHeader is sent as X-PAYMENT.
//
// Outcomes are distinguished by the returned error: a *billingError carries
// the raw HTTP status and code (201 is a success; 402 challenge/refusal, 409
// below_asset_minimum with Extra["minimum_usd"], 422, 502 settlement_pending
// with Extra["transaction"], 503 are all reachable this way).
func (b *httpBilling) Topup(amount, rail, payCurrency, paymentHeader string) (map[string]any, error) {
	return b.TopupWithMode(amount, rail, payCurrency, paymentHeader, "")
}

// TopupWithMode is Topup plus the API's optional `mode` field ("direct" or
// "invoice"). An empty mode sends no field, so the server's own default
// applies; older servers ignore the extra field.
func (b *httpBilling) TopupWithMode(amount, rail, payCurrency, paymentHeader, mode string) (map[string]any, error) {
	body := map[string]any{"amount_usd": amount}
	if rail != "" {
		body["rail"] = rail
	}
	if payCurrency != "" {
		body["pay_currency"] = payCurrency
	}
	if mode != "" {
		body["mode"] = mode
	}
	req, err := b.request(http.MethodPost, "/billing/topup", body)
	if err != nil {
		return nil, err
	}
	if paymentHeader != "" {
		req.Header.Set("X-PAYMENT", paymentHeader)
		// The v2 wire name is PAYMENT-SIGNATURE; the server accepts either, so
		// set both for compatibility and keep X-PAYMENT for older deployments.
		if rail == "x402" {
			req.Header.Set("PAYMENT-SIGNATURE", paymentHeader)
		}
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}

// TopupStatus polls one of the caller's top-up intents.
func (b *httpBilling) TopupStatus(paymentIntentID string) (map[string]any, error) {
	req, err := b.request(http.MethodGet, "/billing/topup/"+url.PathEscape(paymentIntentID), nil)
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}

// Currencies lists the coins the rail currently accepts (public endpoint).
func (b *httpBilling) Currencies() ([]map[string]any, error) {
	req, err := b.request(http.MethodGet, "/billing/currencies", nil)
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authNone, "")
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode currency list: %w", err)
	}
	return out, nil
}

// TopupMinimums reads the public per-rail top-up minimums.
func (b *httpBilling) TopupMinimums(payCurrency string) (map[string]any, error) {
	path := "/billing/topup/minimums"
	if payCurrency != "" {
		path += "?pay_currency=" + url.QueryEscape(payCurrency)
	}
	req, err := b.request(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authNone, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}

// X402Challenge requests an x402 PaymentRequired document without a payment
// signature. The API answers 402; the document is preserved in billingError.Extra.
func (b *httpBilling) X402Challenge(amount string) (map[string]any, error) {
	req, err := b.request(http.MethodGet, "/billing/x402/challenge?amount_usd="+url.QueryEscape(amount), nil)
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		var be *billingError
		if errors.As(err, &be) && be.Status == http.StatusPaymentRequired {
			return be.Extra, nil
		}
		return nil, err
	}
	return decodeBillingObject(data)
}

// Promo redeems a promo code for the authenticated account.
func (b *httpBilling) Promo(code string) (map[string]any, error) {
	req, err := b.request(http.MethodPost, "/billing/promo", map[string]any{"code": code})
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}
