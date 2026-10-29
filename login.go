package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go.sia.tech/core/types"
	indexdapp "go.sia.tech/indexd/api/appclient"
	"go.sia.tech/indexd/slabs"
	siastorage "go.sia.tech/siastorage"
	"golang.org/x/term"
)

// connectSDK builds an SDK instance from the saved config.
func connectSDK(ctx context.Context, cfg *Config) (*siastorage.SDK, func(), error) {
	appKeyBytes, err := hex.DecodeString(cfg.AppKey)
	if err != nil || len(appKeyBytes) < 32 {
		return nil, nil, fmt.Errorf("invalid app key (%d bytes) — run 'tessera login'", len(appKeyBytes))
	}
	appKey := types.PrivateKey(appKeyBytes)

	var appID types.Hash256
	if err := appID.UnmarshalText([]byte(cfg.AppID)); err != nil {
		return nil, nil, fmt.Errorf("invalid app id — run 'tessera login'")
	}

	builder := siastorage.NewBuilder(cfg.IndexerURL, siastorage.AppMetadata{
		ID:          appID,
		Name:        "Tessera CLI",
		Description: "Tessera command-line client",
		ServiceURL:  cfg.IndexerURL,
	})

	sdk, err := builder.SDK(appKey)
	if err != nil {
		return nil, nil, fmt.Errorf("connection failed: %w — try 'tessera login'", err)
	}
	cleanup := func() { sdk.Close() }
	return sdk, cleanup, nil
}

// connectAPI creates a lightweight indexer API client for read-only
// operations. Unlike connectSDK, it does NOT fetch hosts, warm
// connections, or start background refresh loops that increment server
// occupancy on the El Grande pair server.
//
// Use this for commands that only need indexer API access (status, list,
// whoami). Commands that need host connectivity (upload, download, fetch)
// must still use connectSDK.
func connectAPI(cfg *Config) (*indexdapp.Client, types.PrivateKey, error) {
	appKeyBytes, err := hex.DecodeString(cfg.AppKey)
	if err != nil || len(appKeyBytes) < 32 {
		return nil, nil, fmt.Errorf("invalid app key (%d bytes) — run 'tessera login'", len(appKeyBytes))
	}
	appKey := types.PrivateKey(appKeyBytes)
	client := indexdapp.NewClient(cfg.IndexerURL)
	return client, appKey, nil
}

// openBrowser tries to open url in the default browser. It is a package-level
// var so command tests can keep the legacy flow desktop-free (precedent:
// stdinIsTerminal, approveConnectURL).
var openBrowser = func(url string) error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return fmt.Errorf("unsupported platform")
	}
}

// loadOrInitConfig returns the saved config, or a fresh default one when no
// file exists yet: the magic-link path must work on a cold install, like
// signup. A config that exists but cannot be read is an error — billing login
// must not silently replace it.
func loadOrInitConfig() (*Config, error) {
	cfg, err := loadConfig()
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL}, nil
}

// billingLoginFlags resolves the billing flags before the legacy flow runs.
// billing is false when neither --email nor --billing-logout is present: plain
// `tessera login` then takes the unchanged legacy path. --billing-logout wins
// when both are given. Malformed pairs are rejected here rather than silently
// falling through to the interactive legacy prompt.
func billingLoginFlags(args []string) (email, token string, logout, billing bool, err error) {
	if hasFlag(args, "--billing-logout") {
		return "", "", true, true, nil
	}
	if !hasFlag(args, "--email") {
		if hasFlag(args, "--token") {
			return "", "", false, true, errors.New("--token requires --email — usage: tessera login --email <email> --token <token>")
		}
		return "", "", false, false, nil
	}
	email, ok := flagValue(args, "--email")
	if !ok || email == "" {
		return "", "", false, true, errors.New("--email requires an email address — usage: tessera login --email <email> [--token <token>]")
	}
	if hasFlag(args, "--token") {
		token, ok = flagValue(args, "--token")
		if !ok || token == "" {
			return "", "", false, true, errors.New("--token requires the token from the sign-in link — usage: tessera login --email <email> --token <token>")
		}
	}
	return email, token, false, true, nil
}

// billingLogin runs `login --email [--token]`. Without a token it requests a
// magic link (which stores nothing). With a token it exchanges the link for a
// session — skipping the magic-link request, so the per-email cooldown is
// never touched — saves the session, validates it with GET /account and prints
// a short summary. phrase is the positional recovery phrase, passed through to
// the headless storage registration when one has to be minted ("" lets the
// registration flow fall back to TESSERA_PHRASE, then a fresh random phrase).
func billingLogin(email, token, phrase string) error {
	cfg, err := loadOrInitConfig()
	if err != nil {
		return err
	}

	if token == "" {
		if err := newBillingClient(cfg).MagicLink(email); err != nil {
			return err
		}
		printMagicLinkSent(email)
		return nil
	}

	acct, err := verifyBillingLogin(cfg, token)
	if err != nil {
		return loginHint(err)
	}
	printBillingAccountSummary(acct)
	if cfg.AppID == "" || cfg.AppKey == "" {
		// Authenticated but no storage identity yet: register one headlessly.
		// The billing id comes from the /account data fetched just above, so a
		// post-mint failure can name the account it stranded.
		return registerStorageIdentity(cfg, phrase, accountString(acct, "billing_id"))
	}
	return nil
}

// verifyBillingLogin exchanges token for a session, stores it in the 0600
// config, then proves it works with GET /account. The account call uses a
// config copy with the agent key cleared so the freshly verified session is
// unambiguously the credential under test (a stored session already wins, but
// this keeps the check explicit).
func verifyBillingLogin(cfg *Config, token string) (map[string]any, error) {
	session, err := newBillingClient(cfg).Verify(token)
	if err != nil {
		return nil, err
	}
	cfg.BillingSession = session
	if err := saveConfig(cfg); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}
	vc := *cfg
	vc.BillingAgentKey = ""
	return newBillingClient(&vc).Account()
}

// loginHint turns /auth/verify's flat 401 invalid_token into the fix: request
// a fresh link. Everything else passes through unchanged, including the
// client's session-expiry error for a stored cookie that the server rejected.
func loginHint(err error) error {
	var be *billingError
	if errors.As(err, &be) && be.Code == "invalid_token" {
		return fmt.Errorf("the sign-in link is invalid, expired or already used — request a new one with 'tessera login --email <your email>' (%w)", err)
	}
	return err
}

// billingLogout revokes the stored session server-side (best effort — an
// unreachable or already-dead session must not leave the local copy behind)
// and then clears billing_session. An agent key is never touched.
func billingLogout() error {
	cfg, err := loadOrInitConfig()
	if err != nil {
		return err
	}
	if cfg.BillingSession == "" {
		fmt.Println("No billing session is stored; nothing to log out.")
		return nil
	}
	if err := newBillingClient(cfg).Logout(cfg.BillingSession); err != nil {
		fmt.Printf("Server-side revoke failed — clearing the local session anyway: %v\n", err)
	}
	cfg.BillingSession = ""
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Println("Signed out. The local billing session was cleared; an agent key, if any, is untouched.")
	return nil
}

// printMagicLinkSent prints the two-step instruction. The token is in the
// emailed link; nothing is stored until the verify step succeeds.
func printMagicLinkSent(email string) {
	fmt.Println("Magic link requested.")
	fmt.Println()
	fmt.Printf("If %s has a billing account, a sign-in link is on its way.\n", email)
	fmt.Println("Check your inbox, then run:")
	fmt.Println()
	fmt.Printf("  tessera login --email %s --token <token>\n", email)
	fmt.Println()
	fmt.Println("(<token> is the token value in the sign-in link moneyd emailed you.)")
}

// accountString extracts a displayable value from a GET /account map. Missing
// and null fields (period_end and days_left are null when burn is zero) render
// as "".
func accountString(acct map[string]any, key string) string {
	v, ok := acct[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// printBillingAccountSummary prints the short post-login summary. The full
// dashboard is the `account` command; only fields present in the response are
// shown, and no credential is ever echoed.
func printBillingAccountSummary(acct map[string]any) {
	fmt.Println("Signed in.")
	fmt.Println()

	id, kind := accountString(acct, "billing_id"), accountString(acct, "kind")
	if id != "" && kind != "" {
		id += " (" + kind + ")"
	}
	field := func(label, value string) {
		if value != "" {
			fmt.Printf("  %-10s %s\n", label+":", value)
		}
	}
	field("Account", id)
	field("Status", accountString(acct, "status"))
	if bal := accountBalanceUSD(acct, "balance_usd"); bal != "" {
		field("Balance", "$"+bal)
	}
	field("Plan", accountString(acct, "plan"))
	field("Days left", accountString(acct, "days_left"))
	fmt.Println()
	fmt.Println("Session saved. Run 'tessera account' for the full dashboard.")
	fmt.Println("Sign out with 'tessera login --billing-logout'.")
}

// registrationBuilder is the subset of siastorage.Builder the headless
// registration flow drives. The concrete builder is wrapped by
// sdkBuilderAdapter; newSDKBuilder is a package-level seam so tests can run the
// flow network-free (precedent: serviceCommand, newBillingClient).
type registrationBuilder interface {
	RequestConnection(ctx context.Context) (string, error)
	WaitForApproval(ctx context.Context) error
	Register(ctx context.Context, mnemonic string) (registeredApp, error)
}

// registeredApp is what Builder.Register hands back: the derived app key plus
// Close. *siastorage.SDK implements it.
type registeredApp interface {
	AppKey() types.PrivateKey
	Close() error
}

// sdkBuilderAdapter adapts *siastorage.Builder to registrationBuilder; only
// Register needs wrapping because its result type is narrowed.
type sdkBuilderAdapter struct {
	*siastorage.Builder
}

func (a sdkBuilderAdapter) Register(ctx context.Context, mnemonic string) (registeredApp, error) {
	sdk, err := a.Builder.Register(ctx, mnemonic)
	if err != nil {
		return nil, err
	}
	return sdk, nil
}

// newSDKBuilder builds the SDK connection builder for a chosen app ID.
var newSDKBuilder = func(indexerURL string, appID types.Hash256) registrationBuilder {
	return sdkBuilderAdapter{siastorage.NewBuilder(indexerURL, siastorage.AppMetadata{
		ID:          appID,
		Name:        "Tessera CLI",
		Description: "Tessera command-line client",
		ServiceURL:  indexerURL,
	})}
}

// registrationAttempts bounds the post-mint registration retry: one initial
// try plus two retries. The connect key is single-use, so a transient failure
// after the mint would otherwise strand the account; a bounded in-process
// retry can still complete it with the same key and builder.
const registrationAttempts = 3

// registrationRetryBackoff is the pause before retry n (1-based). It is a
// package-level var so tests can zero it and run instantly; the production
// schedule is short (200ms then 500ms).
var registrationRetryBackoff = func(retry int) time.Duration {
	if retry == 1 {
		return 200 * time.Millisecond
	}
	return 500 * time.Millisecond
}

// approveConnectURL completes the headless approval a browser tab normally
// performs: POST {"approve":true} to the SDK's response URL with the connect
// key as the Basic auth password (empty username, per indexd /auth/connect).
// The key travels only in the Authorization header; it is never printed.
var approveConnectURL = func(ctx context.Context, responseURL, connectKey string) error {
	body, err := json.Marshal(map[string]bool{"approve": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("approve connection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("", connectKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("approve connection request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &approveHTTPError{Status: resp.StatusCode}
	}
	return nil
}

// approveHTTPError is a typed non-2xx failure from the headless approval POST.
// It carries the HTTP status so the post-mint retry can tell a definitive 4xx
// client rejection from a transient 5xx/timeout. It is deliberately not a
// billingError: the approval talks to the indexer, not moneyd.
type approveHTTPError struct{ Status int }

func (e *approveHTTPError) Error() string {
	return fmt.Sprintf("approve connection request: HTTP %d", e.Status)
}

// retryableRegistrationError reports whether a failed post-mint registration
// step is worth another attempt. Transport errors, timeouts and the SDK's own
// errors are transient (true); a typed server rejection with a 4xx status is
// definitive (false). The mint runs outside this loop, so identity_exists is
// never classified here.
func retryableRegistrationError(err error) bool {
	var be *billingError
	if errors.As(err, &be) {
		return be.Status >= 500
	}
	var he *approveHTTPError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	// A phrase that does not match the recovered account is a definitive
	// user error: retrying cannot change the derived key.
	if errors.Is(err, errPhraseMismatch) {
		return false
	}
	return true
}

// errIdentityExists is the 409 from POST /credentials: the billing account
// already has a non-revoked storage identity (spec §6.3.1). Recovery now
// fetches the existing identity first, so this is only reachable in a race
// (another device linked the account between the recovery lookup and the
// mint); the guidance points at the recovery-phrase restore.
var errIdentityExists = errors.New("identity_exists: this billing account already has a storage identity — restore it with your 12-word recovery phrase: tessera login --email <email> --token <token> \"<words>\"")

// errRecoveryPhraseRequired stops the flow when the account already has a
// storage identity but no phrase was supplied: minting a fresh phrase there
// would create a second, empty identity under the same account.
var errRecoveryPhraseRequired = errors.New("this account already has a storage identity — provide your 12-word recovery phrase to restore it: tessera login \"<words>\" (or set TESSERA_PHRASE)")

// errPhraseMismatch is returned when the app key derived from the supplied
// phrase does not equal the recovered account's public key. It is definitive
// (never retried) and nothing is persisted.
var errPhraseMismatch = errors.New("that recovery phrase does not match this account")

// isIdentityNotFound reports the 404 from POST /credentials/recovery: the
// billing account has no active storage identity yet, so the flow mints one.
func isIdentityNotFound(err error) bool {
	var be *billingError
	return errors.As(err, &be) && be.Status == http.StatusNotFound && be.Code == "identity_not_found"
}

// storageRegistration reports whether plain `tessera login` should run the
// headless storage registration/recovery instead of the legacy browser flow:
// a billing credential (session or agent key) is stored and no usable app
// credentials exist yet.
func storageRegistration(cfg *Config) bool {
	return (cfg.BillingSession != "" || cfg.BillingAgentKey != "") &&
		(cfg.AppID == "" || cfg.AppKey == "")
}

// storageRegister performs the plain-login storage registration/recovery. The
// GET /account lookup is best-effort only: it lets a post-mint failure name
// the billing account, and any failure falls back to "(unknown)" without
// blocking registration. Split out of cmdLogin so tests can drive the failure
// paths without os.Exit.
func storageRegister(cfg *Config, phraseArg string) error {
	billingID := ""
	if acct, err := newBillingClient(cfg).Account(); err == nil {
		billingID = accountString(acct, "billing_id")
	}
	return registerStorageIdentity(cfg, phraseArg, billingID)
}

// wrapRegistrationIncomplete annotates a failure that happened after POST
// /credentials minted a storage identity. The one-time connect key lived only
// in the process, so the account is left with an identity that was never
// linked; the message says exactly that so the operator knows what state the
// account is in. The underlying error stays retrievable for errors.Is/As, and
// the connect key is never part of either error. billingID is "" when no
// account data was available (the agent-path lookup is best-effort); it then
// renders as "(unknown)".
func wrapRegistrationIncomplete(err error, billingID string) error {
	if billingID == "" {
		billingID = "(unknown)"
	}
	return fmt.Errorf("registration incomplete: the storage identity for billing account %s was minted but not completed; contact support — %w", billingID, err)
}

// registerStorageIdentity performs the headless storage registration or
// recovery: with an authenticated billing credential it first asks the server
// whether this account already has a storage identity.
//
//   - Linked identity: the account is restored from the supplied 12-word
//     phrase. The server returns the identity's ORIGINAL app id, account
//     public key and connect key; the CLI approves the connection with that
//     key and re-registers the phrase under the original app id, re-deriving
//     the same app key. No fresh phrase is ever minted for a linked account,
//     and a phrase whose derived key does not match the account is refused
//     before anything is persisted.
//   - No identity (404): the previous behavior — mint a one-time connect key,
//     approve, register, persist app_id/app_key, activate.
//
// The connect key is never printed. For a fresh identity phraseArg ""
// generates and prints a new phrase (TESSERA_PHRASE is honored like the
// legacy flow). billingID is the GET /account billing_id when the caller
// fetched one ("" otherwise); any failure after a connect key was minted is
// annotated with it. cfg must already carry the authenticated billing
// credential; it is saved on success.
func registerStorageIdentity(cfg *Config, phraseArg, billingID string) error {
	if cfg.BillingSession == "" && cfg.BillingAgentKey == "" {
		return errNoBillingCredentials
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Talk to the server with the credential the user just authenticated as:
	// when a session is stored, keep a possibly different agent key out of
	// this call.
	bc := *cfg
	if bc.BillingSession != "" {
		bc.BillingAgentKey = ""
	}

	// RECOVERY FIRST: ask the server whether this account already has a
	// storage identity. Since v1.11.0 this works with a session OR the
	// account's API key, so a headless machine restores the ORIGINAL App ID
	// instead of minting a second identity. A 404 means no identity exists
	// yet -- mint one.
	appID := siastorage.GenerateAppID()
	var connectKey, expectedPublicKey string
	linked := false
	{
		recAppID, recPublicKey, recConnectKey, recErr := newBillingClient(&bc).RecoveryCredential()
		switch {
		case recErr == nil:
			connectKey = recConnectKey
			expectedPublicKey = recPublicKey
			if recAppID != "" {
				linked = true
				if err := appID.UnmarshalText([]byte(recAppID)); err != nil {
					return fmt.Errorf("recovery: invalid app id from server: %w", err)
				}
			}
		case isIdentityNotFound(recErr):
			// no identity yet -- fall through to mint
		default:
			var be *billingError
			if errors.As(recErr, &be) && be.Code == "account_closed" {
				return errAccountClosed
			}
			return recErr
		}
	}
	if connectKey == "" {
		key, err := newBillingClient(&bc).Credentials()
		if err != nil {
			var be *billingError
			if errors.As(err, &be) && be.Code == "identity_exists" {
				return errIdentityExists
			}
			if errors.As(err, &be) && be.Code == "account_closed" {
				return errAccountClosed
			}
			return err
		}
		connectKey = key
	}
	// connectKey stays in memory: sent only in the Basic header below, never
	// printed or persisted.

	builder := newSDKBuilder(cfg.IndexerURL, appID)

	// The recovery phrase is resolved exactly once, on the first attempt that
	// reaches Register, so a retry never regenerates or reprints it.
	phrase := phraseArg
	if phrase == "" {
		phrase = os.Getenv("TESSERA_PHRASE")
	}
	if linked && phrase == "" {
		// Minting a fresh phrase here would create a second, empty identity
		// under the same account; the caller must supply the one they saved.
		return errRecoveryPhraseRequired
	}
	phraseResolved := false
	resolvePhrase := func() {
		if phraseResolved {
			return
		}
		phraseResolved = true
		if phrase == "" {
			phrase = siastorage.NewSeedPhrase()
			printRecoveryPhrase(phrase)
		} else {
			printSuppliedPhraseNote()
		}
	}

	// The post-mint sequence runs up to registrationAttempts times. Each
	// attempt resumes from the first stage that has not completed, so a
	// successful approval is never repeated and Register keeps using the
	// builder's in-memory shared secret.
	var (
		respURL    string
		approved   bool
		waited     bool
		registered bool
		appKey     types.PrivateKey
	)
	attemptRegistration := func() error {
		if !registered {
			if respURL == "" {
				url, err := builder.RequestConnection(ctx)
				if err != nil {
					return fmt.Errorf("connection request failed: %w", err)
				}
				respURL = url
			}
			if !approved {
				if err := approveConnectURL(ctx, respURL, connectKey); err != nil {
					return err
				}
				approved = true
			}
			if !waited {
				if err := builder.WaitForApproval(ctx); err != nil {
					return fmt.Errorf("wait failed: %w\n  (did the approval go through?)", err)
				}
				waited = true
			}
			resolvePhrase()
			sdk, err := builder.Register(ctx, phrase)
			if err != nil {
				return fmt.Errorf("registration failed: %w", err)
			}
			appKey = sdk.AppKey()
			sdk.Close()
			// A wrong phrase derives a different key: refuse it before it is
			// persisted as this machine's identity.
			if expectedPublicKey != "" {
				pk := appKey.PublicKey()
				if got := "ed25519:" + hex.EncodeToString(pk[:]); got != expectedPublicKey {
					return errPhraseMismatch
				}
			}
			// Optional local encryption of the phrase, exactly like the legacy
			// flow; non-interactive logins skip the prompt entirely.
			if phraseArg == "" && stdinIsTerminal() {
				promptProtectPhrase(cfg, phrase)
			}
			registered = true
		}

		cfg.AppID = appID.String()
		cfg.AppKey = hex.EncodeToString([]byte(appKey))
		if err := saveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		return nil
	}

	var regErr error
	for attempt := 1; attempt <= registrationAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(registrationRetryBackoff(attempt - 1))
		}
		if regErr = attemptRegistration(); regErr == nil {
			break
		}
		if !retryableRegistrationError(regErr) {
			break
		}
	}
	if regErr != nil {
		// A phrase mismatch is a user error, not a stranded identity: the
		// "minted but not completed" wrapper would mislead.
		if errors.Is(regErr, errPhraseMismatch) {
			return regErr
		}
		return wrapRegistrationIncomplete(regErr, billingID)
	}

	if err := newBillingClient(&bc).Activate(); err != nil {
		// A reconcile tick may have linked the identity between Register and
		// activate; already_registered is the desired end state.
		var be *billingError
		if !(errors.As(err, &be) && be.Code == "already_registered") {
			return fmt.Errorf("registered and saved, but activation failed: %w — the identity links on the next billing tick", err)
		}
	}

	fmt.Println()
	fmt.Printf("App ID:   %s\n", cfg.AppID)
	fmt.Printf("Indexer:  %s\n", cfg.IndexerURL)
	fmt.Println("Logged in — storage identity linked.")
	clearReadinessCache()
	acct, acctErr := newBillingClient(&bc).Account()
	if billingID == "" && acctErr == nil {
		billingID = accountString(acct, "billing_id")
	}
	if billingID != "" {
		fmt.Printf("Account:  %s\n", billingID)
	}
	enableSeedLogin(&bc, phrase, billingID)
	if acctErr == nil {
		printReadiness(acct)
	}
	return nil
}

// errAccountClosed: the account was deleted or terminated; no credentials.
var errAccountClosed = errors.New("this account has been deleted or closed; create a new one with 'tessera signup'")

// printRecoveryPhrase prints the one-time banner for a freshly generated
// recovery phrase, shared by the legacy and headless registration flows.
func printRecoveryPhrase(phrase string) {
	fmt.Println(strings.Repeat("═", 54))
	fmt.Println("  RECOVERY PHRASE — SAVE THESE 12 WORDS")
	fmt.Println("  They are the ONLY way to recover your account.")
	fmt.Println("  We do not store them.")
	fmt.Println(strings.Repeat("═", 54))
	fmt.Printf("  %s\n", phrase)
	fmt.Println(strings.Repeat("═", 54))
	fmt.Println()
}

// suppressPhraseNote is set by signup, which prints the phrase itself.
var suppressPhraseNote bool

// printSuppliedPhraseNote explains that a caller-supplied phrase was used
// instead of a freshly generated one.
func printSuppliedPhraseNote() {
	if suppressPhraseNote {
		return
	}
	fmt.Println("Using the recovery phrase you supplied — the app key is derived")
	fmt.Println("from it, so this account is reproducible on any machine.")
	fmt.Println()
}

// promptProtectPhrase is the legacy optional master-password encryption of the
// recovery phrase; the billing flow calls it only when stdin is a terminal.
func promptProtectPhrase(cfg *Config, phrase string) {
	fmt.Print("Protect recovery phrase with a master password? [Y/n] ")
	var protect string
	fmt.Scanln(&protect)
	if strings.ToLower(protect) != "n" {
		pass := readPassword("Master password: ")
		confirm := readPassword("Confirm password:   ")
		if pass != confirm {
			fmt.Println("Passwords don't match. Phrase will NOT be saved locally.")
		} else if pass != "" {
			enc, salt, e := encryptPhrase(phrase, pass)
			if e != nil {
				fmt.Printf("Encryption failed: %v\n", e)
			} else {
				cfg.PhraseEncrypted = enc
				cfg.PhraseSalt = salt
				fmt.Println("Recovery phrase encrypted and saved.")
			}
		}
	}
}

// loginUsage is the one place the login forms are documented.
const loginUsage = `Usage:
  tessera login "<12-word recovery phrase>"       restore your account on this machine
  tessera login --api-key <tsk_...> ["<phrase>"]  log in with your API key (+ phrase)
  tessera login --email <e>                       request a magic-link sign-in email
  tessera login --email <e> --token <t> ["<phrase>"]
                                                  finish the magic-link sign-in
  tessera login --billing-logout                  end the email session
  tessera login --force ...                       replace an existing login on this machine

Your recovery phrase alone is enough: it finds your account and restores the
SAME account id and App ID, with no browser and no password. Quote the 12 words.

New here? Run 'tessera signup' — it creates the account and logs you in.

Env:
  TESSERA_PHRASE    recovery phrase, used like the positional phrase argument

This command does not run, link an identity, or print a recovery phrase
when only --help/-h is given.`

// cmdLogin connects this machine to Tessera. Every form is headless; the old
// browser connect-key ("app password") flow was removed in v1.11.0 because it
// silently created a NEW, empty storage identity (random App ID) for people
// trying to restore an existing account.
//
// The billing paths use moneyd's /api/v1/auth endpoints. Production fronts
// them with Cloudflare (spec §6.1/§10); /api/v1/auth/* must stay reachable by
// a plain HTTP client.
func cmdLogin(args []string) {
	// Side-effect free out: must be checked before anything else, so
	// `tessera login --help` can never mint an identity or print a phrase.
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println(loginUsage)
		return
	}
	if err := runLogin(args); err != nil {
		fatal("%v", err)
	}
}

// errAlreadyLoggedIn protects an existing login from being replaced silently.
var errAlreadyLoggedIn = errors.New("this machine is already logged in (see 'tessera whoami'); run 'tessera logout' first, or pass --force")

// runLogin is cmdLogin without os.Exit, so tests can drive every path.
func runLogin(args []string) error {
	phraseArg := ""
	if pos := positional(args); len(pos) > 0 {
		phraseArg = strings.Join(pos, " ")
	}
	if phraseArg == "" {
		phraseArg = os.Getenv("TESSERA_PHRASE")
	}
	force := hasFlag(args, "--force")

	email, token, logout, billing, err := billingLoginFlags(args)
	if err != nil {
		return err
	}
	if logout {
		return billingLogout()
	}
	if billing {
		return billingLogin(email, token, phraseArg)
	}

	prev, err := loadOrInitConfig()
	if err != nil {
		return err
	}
	loggedIn := prev.AppID != "" && prev.AppKey != ""

	if apiKey, ok := flagValue(args, "--api-key"); ok {
		if apiKey == "" || !strings.HasPrefix(apiKey, "tsk_") {
			return errors.New("--api-key needs your tsk_... API key")
		}
		if loggedIn && !force {
			return errAlreadyLoggedIn
		}
		cfg := freshConfig(prev)
		cfg.BillingAgentKey = apiKey
		acct, err := newBillingClient(cfg).Account()
		if err != nil {
			return fmt.Errorf("API key rejected: %s", describeBillingError(err))
		}
		if err := saveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		return registerStorageIdentity(cfg, phraseArg, accountString(acct, "billing_id"))
	}

	// A billing credential with no storage identity yet (signup interrupted,
	// or an email login): finish the headless registration/recovery.
	if storageRegistration(prev) {
		return storageRegister(prev, phraseArg)
	}

	if phraseArg != "" {
		if loggedIn && !force {
			return errAlreadyLoggedIn
		}
		return seedLogin(freshConfig(prev), phraseArg)
	}

	if loggedIn {
		fmt.Println("Already logged in.")
		fmt.Printf("App ID:   %s\n", prev.AppID)
		if acct, err := newBillingClient(prev).Account(); err == nil {
			if id := accountString(acct, "billing_id"); id != "" {
				fmt.Printf("Account:  %s\n", id)
			}
			printReadiness(acct)
		}
		return nil
	}
	return errors.New("nothing to log in with.\n\n" + loginUsage)
}

// cmdLogout removes this machine's local credentials. The account and its
// files are untouched; log back in with your recovery phrase.
//
//	tessera logout [--yes]
func cmdLogout(args ...string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println("Usage: tessera logout [--yes]\n\nRemoves local credentials (~/.tessera/config.json). Your account and files stay;\nlog back in any time with: tessera login \"<12-word recovery phrase>\"")
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Not logged in.")
		return
	}
	fmt.Printf("App ID: %s\n", cfg.AppID)
	if !hasFlag(args, "--yes") {
		if !stdinIsTerminal() {
			fatal("refusing to remove credentials without confirmation; pass --yes")
		}
		fmt.Print("Remove local credentials? [y/N] ")
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(a) != "y" {
			fmt.Println("Aborted.")
			return
		}
	}
	if cfg.BillingSession != "" {
		_ = newBillingClient(cfg).Logout(cfg.BillingSession)
	}
	if err := deleteConfigFiles(); err != nil {
		fatal("logout: %v", err)
	}
	fmt.Println("Logged out. Local credentials removed.")
	fmt.Println("Log back in with: tessera login \"<12-word recovery phrase>\"")
}

// cmdPhrase reveals the locally-saved recovery phrase, decrypted with the
// master password set when it was originally saved during 'tessera login'.
// This is the only local copy: the phrase itself is never sent to or
// stored by the server, so if it wasn't saved (or the password is lost)
// there is no recovery path other than the phrase the user wrote down.
func cmdPhrase(args []string) {
	pos := positional(args)
	sub := "show"
	if len(pos) > 0 {
		sub = pos[0]
	}
	if hasFlag(args, "--help") || hasFlag(args, "-h") || sub != "show" {
		fmt.Println("Usage: tessera phrase show   Reveal the locally-saved recovery phrase")
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	if cfg.PhraseEncrypted == "" || cfg.PhraseSalt == "" {
		fmt.Println("No recovery phrase is saved locally for this login.")
		fmt.Println("(It was either not protected with a master password at login")
		fmt.Println("time, or this identity was set up without one -- e.g. TESSERA_PHRASE.)")
		return
	}

	password := readPassword("Master password: ")
	phrase, err := decryptPhrase(cfg.PhraseEncrypted, cfg.PhraseSalt, password)
	if err != nil {
		fatal("wrong password, or the saved phrase is corrupted: %v", err)
	}

	fmt.Println()
	fmt.Println(strings.Repeat("═", 54))
	fmt.Println("  RECOVERY PHRASE")
	fmt.Println(strings.Repeat("═", 54))
	fmt.Printf("  %s\n", phrase)
	fmt.Println(strings.Repeat("═", 54))
	fmt.Println()
	fmt.Println("Use this to connect another device/app to this same account")
	fmt.Println("(e.g. Tessera Desktop's \"I already have an account\").")
}

func cmdWhoami() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	fmt.Printf("Indexer:  %s\n", cfg.IndexerURL)
	fmt.Printf("App ID:   %s\n", cfg.AppID)
	if pub := appKeyPublicHex(cfg.AppKey); pub != "" {
		fmt.Printf("App Key:  %s (public key; private key saved locally)\n", pub)
	} else {
		fmt.Println("App Key:  (missing)")
	}
	if cfg.PhraseEncrypted != "" {
		fmt.Println("Recovery: encrypted phrase saved (password-protected)")
	} else {
		fmt.Println("Recovery: phrase NOT saved locally")
	}
}

func cmdStatus(args ...string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, appKey, err := connectAPI(cfg)
	if err != nil {
		fatal("%v", err)
	}

	acct, err := client.Account(ctx, appKey)
	if err != nil {
		fatal("account: %v", err)
	}

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	idx, err := rebuildIndex(ctx, NewRemote(sdk), cfg.AppID)
	if err != nil {
		fatal("index: %v", err)
	}

	roots, _ := listSyncRoots()
	var pending int
	for _, r := range roots {
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		for _, e := range st.Entries {
			if e.DeletedLocal || e.DeletedRemote {
				pending++
			}
		}
	}

	var billingAcct map[string]any
	if credentialLabel(cfg) != "" {
		billingAcct, _ = newBillingClient(cfg).Account()
	}
	if hasFlag(args, "--json") {
		out := map[string]interface{}{
			"ready":        acct.Ready,
			"ready_note":   readyNoteFor(acct.Ready, readinessState(billingAcct)),
			"indexer":      cfg.IndexerURL,
			"app_id":       cfg.AppID,
			"files":        len(idx.Paths),
			"bytes":        idx.TotalSize(),
			"sync_folders": len(roots),
			"pending_sync": pending,
		}
		if r, ok := billingAcct["readiness"]; ok {
			out["readiness"] = r
		}
		if id := accountString(billingAcct, "billing_id"); id != "" {
			out["account_id"] = id
		}
		emitJSON(out)
		return
	}

	if id := accountString(billingAcct, "billing_id"); id != "" {
		fmt.Printf("Account:   %s\n", id)
	}
	fmt.Printf("App ID:    %s\n", cfg.AppID)
	if state, msg := accountReadiness(billingAcct); state != "" {
		fmt.Printf("Status:    %s\n", readinessLabel(state))
		if msg != "" {
			fmt.Printf("           %s\n", msg)
		}
	}
	fmt.Printf("Host funding ready: %v\n", acct.Ready)
	if !acct.Ready {
		fmt.Printf("           (%s)\n", readyNoteFor(acct.Ready, readinessState(billingAcct)))
	}
	fmt.Printf("Files:     %d\n", len(idx.Paths))
	fmt.Printf("Total:     %s\n", formatBytes(idx.TotalSize()))
	if len(roots) > 0 {
		fmt.Printf("Synced:    %d folder(s)", len(roots))
		if pending > 0 {
			fmt.Printf(", %d change(s) pending — run 'tessera sync'", pending)
		}
		fmt.Println()
	}
}

func sameCursor(a, b slabs.Cursor) bool {
	return a.Key == b.Key && a.After.Equal(b.After)
}

// readyNote explains what the server's Ready flag means. indexd sets Ready once
// the account's host funding passes its repair-redundancy threshold
// (accounts/manager.go ReadyHostThreshold). A free agent account stays
// Ready:false until its first paid top-up, even though Tessera funds enough
// hosts for its free first upload. Whether a transfer can run right now is
// decided by the CLI's own readiness check (readiness.go), which waits for
// host funding to land and explains any blocker.
// readinessState is the billing readiness state ("ready", "free_trial", ...)
// or "" when the account endpoint did not report one.
func readinessState(billingAcct map[string]any) string {
	state, _ := accountReadiness(billingAcct)
	return state
}

// readyNoteFor explains Ready:false in terms of the billing state: a funded
// account is only waiting for indexd to fund hosts, an unfunded one stays
// born-paused until its first top-up.
func readyNoteFor(ready bool, state string) string {
	if !ready && state == "funded" {
		return "funded: indexd is funding hosts for this account, which usually " +
			"takes a few minutes after a top-up. Uploads wait for it automatically"
	}
	return readyNote(ready)
}

func readyNote(ready bool) string {
	if ready {
		return "host funding is past the repair-redundancy threshold"
	}
	return "expected for a free-trial or unfunded account: indexd only funds " +
		"hosts itself after the first top-up. It is not an error — see Status " +
		"above for what this account can do now"
}

func readPassword(prompt string) string {
	fmt.Print(prompt)
	pass, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return ""
	}
	return string(pass)
}

func fatal(format string, args ...interface{}) {
	transferProgress.Finish() // clear a live progress line first
	msg := fmt.Sprintf(format, args...)
	msg = explainFatalHostShortage(msg)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprintf(os.Stderr, "ERROR: %s", msg)
	os.Exit(1)
}
