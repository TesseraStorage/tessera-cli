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

// Seed-only login.
//
// The 12-word recovery phrase alone is enough. A second ed25519 key — the
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

// purgeDataAPI is the Sonnet packet "client-actions-audit-and-purge-data"
// (2026-11-16) user-initiated purge-data command: deletes every object and
// unpins every slab but keeps the account, connect key, and balance.
type purgeDataAPI interface {
	PurgeData() (map[string]any, error)
}

var _ recoveryAPI = (*httpBilling)(nil)
var _ accountDeleteAPI = (*httpBilling)(nil)
var _ purgeDataAPI = (*httpBilling)(nil)

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

// PurgeData calls moneyd's user-initiated purge-data endpoint. Works at any
// balance, including ≤ $0 and grace; leaves the account, connect key, and
// balance untouched (Sonnet packet "client-actions-audit-and-purge-data",
// 2026-11-16).
func (b *httpBilling) PurgeData() (map[string]any, error) {
	req, err := b.request(http.MethodPost, "/account/purge-data", nil)
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
  - Or sign in another way (email):
      tessera login --email <you@example.com>`)

// seedLogin restores an account from the phrase alone (no browser, no API key).
func seedLogin(cfg *Config, phrase string) error {
	if n := len(strings.Fields(phrase)); n != 12 {
		return fmt.Errorf("a recovery phrase has 12 words (got %d); quote it: tessera login \"word1 word2 ...\"", n)
	}
	if err := checkPhraseFormat(phrase); err != nil {
		return err
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
	printTrialBlock(buildTrialView(acct), "", 9)
}

func readinessLabel(state string) string {
	switch state {
	case "funded":
		return "ready"
	case "free_trial":
		return "technical validation"
	case "free_trial_active":
		return "technical validation (upload used)"
	case "top_up_required":
		return "top up to continue"
	case "setup_incomplete":
		return "still getting ready (storage identity not linked yet)"
	case "paused":
		return "paused by support"
	case "suspended":
		return "suspended — files deleted; top up to continue"
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
	// ShownState/ShownAt record the last notice actually printed, so the
	// same line is not repeated on every command.
	ShownState string    `json:"shown_state,omitempty"`
	ShownAt    time.Time `json:"shown_at,omitempty"`
}

// readinessNoticeRepeat is how long an unchanged readiness line stays quiet
// after it was printed once.
const readinessNoticeRepeat = 6 * time.Hour

func readinessCachePath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "readiness-cache.json")
}

// readinessNotice returns a one-line stderr notice when the account needs
// action (top up, finish setup, paused) ("" when it is, or when nothing is known). Cached for 5 min so
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
		if v := buildTrialView(acct); v != nil {
			// TT-nov5 H: one short line elsewhere; full banner stays on account/status.
			if v.ShortBanner != "" {
				c.Message = v.ShortBanner
			} else {
				c.Message = v.UploadsLine
			}
		}
		c.CheckedAt = time.Now()
		if p != "" {
			data, _ := json.Marshal(c)
			_ = os.WriteFile(p, data, 0o600)
		}
	}
	// Only states that need action get the line; a working technical validation is
	// explained by signup/status/account/usage and never repeated (TT retest 3).
	if !readinessNeedsAction(c.State) {
		return ""
	}
	if c.ShownState == c.State && time.Since(c.ShownAt) < readinessNoticeRepeat {
		return ""
	}
	c.ShownState, c.ShownAt = c.State, time.Now()
	if p != "" {
		data, _ := json.Marshal(c)
		_ = os.WriteFile(p, data, 0o600)
	}
	return "Account: " + readinessLabel(c.State) + ". " + c.Message
}

// readinessNeedsAction is true for states where an upload cannot run.
func readinessNeedsAction(state string) bool {
	switch state {
	case "", "funded", "free_trial", "free_trial_active":
		return false
	}
	return true
}

// clearReadinessCache forces the next notice to re-check (after topup/login).
func clearReadinessCache() {
	if p := readinessCachePath(); p != "" {
		_ = os.Remove(p)
	}
}

// ---------------------------------------------------------------- account delete

// cmdAccountDelete: tessera account delete [--yes] [--json]
func cmdAccountPurgeData(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println(`Usage: tessera account purge-data [--yes] [--json]

Permanently deletes ALL of your files right now:
  - every object is deleted and every slab is unpinned (cannot be undone),
  - works at any balance, including $0 or in the grace period.
Your account, connect key, and balance are NOT affected -- this is NOT the
same as "tessera account delete". Use --yes to skip the confirmation
prompt (agents, scripts).`)
		return
	}
	cfg, err := loadConfig()
	if err != nil || credentialLabel(cfg) == "" {
		fatal("%v", errNoBillingCredentials)
	}
	client := newBillingClient(cfg)
	if !hasFlag(args, "--yes") {
		if !stdinIsTerminal() {
			fatal("refusing to purge all data without confirmation; pass --yes (non-interactive)")
		}
		fmt.Fprintln(os.Stderr, "This permanently deletes ALL of your files. This cannot be undone.")
		fmt.Fprintln(os.Stderr, "Your account, connect key, and balance are kept.")
		fmt.Fprint(os.Stderr, "Type y to confirm: ")
		var typed string
		fmt.Scanln(&typed)
		if strings.TrimSpace(typed) != "y" {
			fatal("confirmation did not match; nothing was purged")
		}
	}
	pd, ok := client.(purgeDataAPI)
	if !ok {
		fatal("billing client does not support purge-data")
	}
	res, err := pd.PurgeData()
	if err != nil {
		fatal("%s", describeBillingError(err))
	}
	// Every object is gone, trashed ones included, so the local trash list
	// and file index would only point at deleted data.
	clearLocalFileState()
	if hasFlag(args, "--json") {
		emitJSON(res)
		return
	}
	objects, _ := res["objects_removed"].(float64)
	slabsRemoved, _ := res["slabs_removed"].(float64)
	fmt.Printf("Purged %d object(s), unpinned %d slab(s).\n", int64(objects), int64(slabsRemoved))
	fmt.Println("The trash and this machine's file index were cleared too.")
	if _, sentence, ok := accountStanding(res); ok {
		fmt.Println(sentence)
	}
}

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
		fmt.Fprintf(os.Stderr, "Your remaining balance (%s) is forfeited and will NOT be refunded.\n", signedMoney(bal))
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
	dir, _ := configDir()
	removeAccountLocalState()
	if hasFlag(args, "--json") {
		emitJSON(res)
	} else {
		fmt.Printf("Account %s deleted.\n", id)
		if v := accountString(res, "balance_forfeited_usd"); v != "" {
			fmt.Printf("  balance forfeited: %s (no refunds)\n", signedMoney(v))
		}
		fmt.Println("  Stored files are being removed from the network.")
		fmt.Printf("  This machine's login, file index, trash list and update-check cache were removed from %s.\n", dir)
	}
	// The update notice runs now, and the local state is removed once more
	// afterwards, so nothing about the deleted account is left behind.
	flushNotices()
	removeAccountLocalState()
	os.Exit(0)
}

// removeAccountLocalState deletes everything in ~/.tessera that belongs to
// the account after 'tessera account delete': the login (config.json), the
// readiness cache, the local file index (index.json and its lock), the
// trash list (trash.json) and the update-check cache. A signup-secrets file
// is left alone: it is the user's own copy and 'tessera secrets done'
// removes it. The directory itself is removed when it is then empty.
func removeAccountLocalState() {
	clearLocalFileState()
	_ = deleteConfigFiles()
	if p, err := updateCachePath(); err == nil {
		_ = os.Remove(p)
	}
	if dir, err := configDir(); err == nil {
		_ = os.Remove(dir) // only removes it if empty
	}
}

// clearLocalFileState removes the local file index (and its lock) and the
// trash list. Both are caches of the account's remote objects; the index is
// rebuilt from the network by the next command that needs it.
func clearLocalFileState() {
	if p, err := indexPath(); err == nil {
		_ = os.Remove(p)
		_ = os.Remove(p + ".lock")
	}
	if p, err := trashIndexPath(); err == nil {
		_ = os.Remove(p)
	}
}
