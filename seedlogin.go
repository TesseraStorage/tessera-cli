package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

// Seed-only login (v1.11.0).
//
// Before v1.11.0 a machine without ~/.tessera could only get back into an
// account with the API key or an email session. `tessera login "<phrase>"`
// fell through to the old browser connect-key ("app password") flow, which
// minted a NEW random App ID — a different, empty storage identity.
//
// Now the 12-word recovery phrase alone is enough. A second ed25519 key — the
// recovery key — is derived from the phrase (never from anything else) and its
// PUBLIC half is registered with the billing server whenever this CLI has the
// phrase and a working credential (signup, login). On a fresh machine the CLI
// signs a timestamped challenge with it; the server answers with a fresh API
// key for the account, and the normal recovery path restores the ORIGINAL App
// ID and re-derives the same app key from the phrase. The phrase itself never
// leaves this machine.

const recoveryKeyDomain = "tessera recovery key v1\x00"

// normalizePhrase lowercases and collapses whitespace so "Word  word" and
// "word word" derive the same key.
func normalizePhrase(phrase string) string {
	return strings.Join(strings.Fields(strings.ToLower(phrase)), " ")
}

// deriveRecoveryKey derives the phrase's recovery signing key:
// ed25519 seed = BLAKE2b-256(domain || normalized phrase).
func deriveRecoveryKey(phrase string) ed25519.PrivateKey {
	seed := blake2b.Sum256(append([]byte(recoveryKeyDomain), normalizePhrase(phrase)...))
	return ed25519.NewKeyFromSeed(seed[:])
}

func recoveryPublicHex(sk ed25519.PrivateKey) string {
	return hex.EncodeToString(sk.Public().(ed25519.PublicKey))
}

// recoveryAPI is the optional moneyd surface for seed login and account
// deletion; separate from billingAPI so existing fakes keep compiling.
type recoveryAPI interface {
	RegisterRecoveryKey(publicKey string, ts int64, signature string) error
	RecoverWithKey(publicKey string, ts int64, signature string) (accountID, kind, apiKey string, err error)
}

type accountDeleteAPI interface {
	DeleteAccount(confirmAccountID string) (map[string]any, error)
}

var _ recoveryAPI = (*httpBilling)(nil)
var _ accountDeleteAPI = (*httpBilling)(nil)

func (b *httpBilling) RegisterRecoveryKey(publicKey string, ts int64, signature string) error {
	req, err := b.request(http.MethodPost, "/credentials/recovery-key", map[string]any{
		"recovery_public_key": publicKey, "timestamp": ts, "signature": signature,
	})
	if err != nil {
		return err
	}
	_, _, err = b.do(req, authAuto, "")
	return err
}

func (b *httpBilling) RecoverWithKey(publicKey string, ts int64, signature string) (string, string, string, error) {
	req, err := b.request(http.MethodPost, "/auth/recover", map[string]any{
		"recovery_public_key": publicKey, "timestamp": ts, "signature": signature,
	})
	if err != nil {
		return "", "", "", err
	}
	_, data, err := b.do(req, authNone, "")
	if err != nil {
		return "", "", "", err
	}
	obj, err := decodeBillingObject(data)
	if err != nil {
		return "", "", "", err
	}
	accountID, _ := obj["account_id"].(string)
	kind, _ := obj["kind"].(string)
	apiKey, _ := obj["api_key"].(string)
	if accountID == "" || apiKey == "" {
		return "", "", "", fmt.Errorf("recover response is missing account_id or api_key")
	}
	return accountID, kind, apiKey, nil
}

func (b *httpBilling) DeleteAccount(confirmAccountID string) (map[string]any, error) {
	req, err := b.request(http.MethodPost, "/account/delete", map[string]any{"confirm_account_id": confirmAccountID})
	if err != nil {
		return nil, err
	}
	_, data, err := b.do(req, authAuto, "")
	if err != nil {
		return nil, err
	}
	return decodeBillingObject(data)
}

// nowUnix is a seam for tests.
var nowUnix = func() int64 { return time.Now().Unix() }

// registerRecoveryKey enables seed-only login for billingID. Best effort: a
// failure is reported but never fails the surrounding login/signup.
func registerRecoveryKey(cfg *Config, phrase, billingID string) error {
	if phrase == "" || billingID == "" {
		return errors.New("missing phrase or account id")
	}
	api, ok := newBillingClient(cfg).(recoveryAPI)
	if !ok {
		return errors.New("billing client does not support recovery keys")
	}
	sk := deriveRecoveryKey(phrase)
	pk := recoveryPublicHex(sk)
	ts := nowUnix()
	msg := fmt.Sprintf("tessera-recovery-key-register:v1:%s:%s:%d", billingID, pk, ts)
	sig := hex.EncodeToString(ed25519.Sign(sk, []byte(msg)))
	return api.RegisterRecoveryKey(pk, ts, sig)
}

// enableSeedLogin registers the recovery key and prints one line about it.
func enableSeedLogin(cfg *Config, phrase, billingID string) {
	if err := registerRecoveryKey(cfg, phrase, billingID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not enable seed-phrase login yet (%s).\n", describeBillingError(err))
		fmt.Fprintln(os.Stderr, "  Run 'tessera login --api-key <key> \"<phrase>\"' later to enable it.")
		return
	}
	fmt.Println("Seed-phrase login enabled: 'tessera login \"<12 words>\"' restores this account on any machine.")
}

// errSeedNotRegistered: the phrase is valid-looking but the server knows no
// account for it.
var errSeedNotRegistered = errors.New(`no account is registered for this recovery phrase.
  - Check the 12 words (order matters).
  - Accounts created before tessera v1.11.0 need seed login enabled once:
      tessera login --api-key <your tsk_ API key> "<12 words>"
    or sign in by email:
      tessera login --email <you@example.com>
  After that, 'tessera login "<12 words>"' works on any machine`)

// seedLogin restores an account from the phrase alone (no browser, no API key).
func seedLogin(cfg *Config, phrase string) error {
	if n := len(strings.Fields(phrase)); n != 12 {
		return fmt.Errorf("a recovery phrase has 12 words (got %d); quote it: tessera login \"word1 word2 ...\"", n)
	}
	api, ok := newBillingClient(cfg).(recoveryAPI)
	if !ok {
		return errors.New("billing client does not support seed login")
	}
	sk := deriveRecoveryKey(phrase)
	pk := recoveryPublicHex(sk)
	ts := nowUnix()
	msg := fmt.Sprintf("tessera-recover:v1:%s:%d", pk, ts)
	accountID, _, apiKey, err := api.RecoverWithKey(pk, ts, hex.EncodeToString(ed25519.Sign(sk, []byte(msg))))
	if err != nil {
		var be *billingError
		if errors.As(err, &be) {
			switch be.Code {
			case "recovery_key_not_registered":
				return errSeedNotRegistered
			case "account_closed":
				return errors.New("this account has been deleted or closed; it cannot be restored")
			case "recovery_failed":
				return errors.New("seed login was rejected (check this machine's clock is correct, then retry)")
			}
		}
		return err
	}
	fmt.Printf("Account:  %s (found from your recovery phrase)\n", accountID)
	cfg.BillingAgentKey = apiKey
	cfg.BillingSession = ""
	cfg.AppID, cfg.AppKey = "", ""
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return registerStorageIdentity(cfg, phrase, accountID)
}

// freshConfig keeps only non-credential settings from prev.
func freshConfig(prev *Config) *Config {
	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL}
	if prev != nil {
		if prev.BillingAPIURL != "" {
			cfg.BillingAPIURL = prev.BillingAPIURL
		}
		cfg.UpdateCheckDisabled = prev.UpdateCheckDisabled
		cfg.VersionRetention = prev.VersionRetention
		cfg.TrashRetentionDays = prev.TrashRetentionDays
		cfg.DefaultJobs = prev.DefaultJobs
	}
	return cfg
}

// ---------------------------------------------------------------- readiness

// accountReadiness pulls moneyd's plain-language readiness block out of a
// GET /account response.
func accountReadiness(acct map[string]any) (state, message string) {
	r, _ := acct["readiness"].(map[string]any)
	if r == nil {
		return "", ""
	}
	state, _ = r["state"].(string)
	message, _ = r["message"].(string)
	return state, message
}

func printReadiness(acct map[string]any) {
	state, msg := accountReadiness(acct)
	if state == "" {
		return
	}
	fmt.Printf("Status:   %s\n", readinessLabel(state))
	if msg != "" {
		fmt.Printf("          %s\n", msg)
	}
}

func readinessLabel(state string) string {
	switch state {
	case "funded":
		return "ready"
	case "free_trial":
		return "free trial — first upload is prepaid"
	case "free_trial_active":
		return "free trial"
	case "top_up_required":
		return "top up to continue"
	case "setup_incomplete":
		return "still getting ready (storage identity not linked yet)"
	case "paused":
		return "paused by support"
	case "suspended":
		return "suspended — top up to continue"
	case "closed":
		return "closed"
	}
	return state
}

// readinessNoticeTTL throttles the per-command readiness line.
const readinessNoticeTTL = 5 * time.Minute

type readinessCache struct {
	CheckedAt time.Time `json:"checked_at"`
	State     string    `json:"state"`
	Message   string    `json:"message"`
}

func readinessCachePath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "readiness-cache.json")
}

// readinessNotice returns a one-line stderr notice when the account is not
// fully ready ("" when it is, or when nothing is known). Cached for 5 min so
// it never adds a network call to every command.
func readinessNotice(cfg *Config) string {
	if cfg == nil || credentialLabel(cfg) == "" {
		return ""
	}
	p := readinessCachePath()
	var c readinessCache
	if p != "" {
		if data, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(data, &c)
		}
	}
	if time.Since(c.CheckedAt) >= readinessNoticeTTL {
		acct, err := newBillingClient(cfg).Account()
		if err != nil {
			return ""
		}
		c.State, c.Message = accountReadiness(acct)
		c.CheckedAt = time.Now()
		if p != "" {
			data, _ := json.Marshal(c)
			_ = os.WriteFile(p, data, 0o600)
		}
	}
	if c.State == "" || c.State == "funded" || c.State == "free_trial_active" {
		return ""
	}
	return "Account: " + readinessLabel(c.State) + ". " + c.Message
}

// clearReadinessCache forces the next notice to re-check (after topup/login).
func clearReadinessCache() {
	if p := readinessCachePath(); p != "" {
		_ = os.Remove(p)
	}
}

// ---------------------------------------------------------------- account delete

// cmdAccountDelete: tessera account delete [--yes] [--json]
func cmdAccountDelete(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println(`Usage: tessera account delete [--yes] [--json]

Permanently deletes your Tessera account:
  - all API keys are revoked and every session is signed out,
  - every stored file is deleted from the network (cannot be undone),
  - billing stops and the account is marked deleted.
Any remaining balance is forfeited — it is NOT refunded.

You are asked to type your account id to confirm. Agents can pass --yes
to skip the prompt. Local credentials (~/.tessera) are removed afterwards.`)
		return
	}
	cfg, err := loadConfig()
	if err != nil || credentialLabel(cfg) == "" {
		fatal("%v", errNoBillingCredentials)
	}
	client := newBillingClient(cfg)
	acct, err := client.Account()
	if err != nil {
		fatal("%s", describeBillingError(err))
	}
	id := accountString(acct, "billing_id")
	if id == "" {
		fatal("could not read your account id")
	}
	fmt.Fprintf(os.Stderr, "This permanently deletes account %s and ALL of its files.\n", id)
	if bal := accountString(acct, "balance_usd"); bal != "" {
		fmt.Fprintf(os.Stderr, "Your remaining balance ($%s) is forfeited and will NOT be refunded.\n", bal)
	}
	if !hasFlag(args, "--yes") {
		if !stdinIsTerminal() {
			fatal("refusing to delete without confirmation; pass --yes (non-interactive)")
		}
		fmt.Fprintf(os.Stderr, "Type the account id (%s) to confirm: ", id)
		var typed string
		fmt.Scanln(&typed)
		if strings.TrimSpace(typed) != id {
			fatal("confirmation did not match; nothing was deleted")
		}
	}
	del, ok := client.(accountDeleteAPI)
	if !ok {
		fatal("billing client does not support account deletion")
	}
	res, err := del.DeleteAccount(id)
	if err != nil {
		fatal("%s", describeBillingError(err))
	}
	_ = deleteConfigFiles()
	if hasFlag(args, "--json") {
		emitJSON(res)
		return
	}
	fmt.Printf("Account %s deleted.\n", id)
	if v := accountString(res, "balance_forfeited_usd"); v != "" {
		fmt.Printf("  balance forfeited: $%s (no refunds)\n", v)
	}
	fmt.Println("  stored files are being removed from the network; local credentials were removed.")
}
