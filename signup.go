package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	siastorage "go.sia.tech/siastorage"
)

// newBillingClient builds the moneyd client a command talks to. It is a
// package-level var (precedent: serviceCommand) so command tests can inject an
// in-memory fake and stay network-free.
var newBillingClient = func(cfg *Config) billingAPI {
	return newBillingAPI(cfg)
}

// signupResult is the server's one-time response to an agent signup plus
// what the automatic login produced.
type signupResult struct {
	AccountID string
	APIKey    string
	Phrase    string
	AppID     string
	Readiness map[string]any
}

// errSignupKeyExists stops a second signup from silently replacing the
// credentials stored on this machine; --force opts in to a new account.
var errSignupKeyExists = errors.New("this machine is already logged in to a Tessera account; pass --force to create a new account (the old one stays, log back in with its recovery phrase)")

// signupRegister is the automatic login after signup (a seam for tests).
var signupRegister = registerStorageIdentity

// cmdSignup creates a new agent account AND logs this machine in.
//
//	tessera signup [--name <name>] [--contact-email <email>] [--force] [--json]
//
// v1.11.0: signup used to stop after creating the billing account, so users
// had to run 'tessera login' next. Worse, a stale email session left in the
// config won over the new API key, so the follow-up login could bind the new
// App ID to a different (old) account. Signup now always starts from a clean
// config, creates the storage identity in the same step and registers the
// seed-login key, then prints the two things to keep: the recovery phrase
// and the API key. This is the one command that deliberately prints secrets,
// exactly once.
func cmdSignup(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println("Usage: tessera signup [--name <name>] [--contact-email <email>] [--force] [--json]")
		fmt.Println()
		fmt.Println("Creates a new account and logs this machine in (no separate 'tessera login').")
		fmt.Println("Prints your 12-word recovery phrase and API key ONCE — keep both safe.")
		fmt.Println("The phrase alone restores the account later: tessera login \"<12 words>\"")
		fmt.Println("This command does not run when only --help/-h is given.")
		return
	}
	asJSON := hasFlag(args, "--json")
	result, err := signup(args, asJSON)
	if err != nil && result.APIKey == "" {
		fatal("%v", err)
	}
	// Print on success, and also when the account was created but a later
	// step failed: the server will never show the key again.
	printSignupResult(result, asJSON)
	if err != nil {
		fatal("%v", err)
	}
}

// signup does the command's real work without exiting. Registration output is
// sent to stderr in --json mode so stdout stays one JSON object.
func signup(args []string, asJSON bool) (signupResult, error) {
	name, _ := flagValue(args, "--name")
	contactEmail, _ := flagValue(args, "--contact-email")

	prev, err := loadConfig()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A config that exists but cannot be read (corrupt, unsupported
			// indexer) must not be silently replaced by a fresh one.
			return signupResult{}, fmt.Errorf("read config: %w", err)
		}
		prev = nil
	}
	if prev != nil && (prev.BillingAgentKey != "" || prev.BillingSession != "" || prev.AppKey != "") && !hasFlag(args, "--force") {
		return signupResult{}, errSignupKeyExists
	}
	// Always a clean config: never carry an old session/key/App ID over.
	cfg := freshConfig(prev)
	result, err := createAgentAccount(newBillingClient(cfg), cfg, name, contactEmail)
	if err != nil {
		return result, err
	}
	result.Phrase = os.Getenv("TESSERA_PHRASE")
	if result.Phrase == "" {
		result.Phrase = siastorage.NewSeedPhrase()
	}
	restore := func() {}
	if asJSON {
		orig := os.Stdout
		os.Stdout = os.Stderr
		restore = func() { os.Stdout = orig }
	}
	suppressPhraseNote = true
	err = signupRegister(cfg, result.Phrase, result.AccountID)
	suppressPhraseNote = false
	restore()
	result.AppID = cfg.AppID
	if acct, aerr := newBillingClient(cfg).Account(); aerr == nil {
		result.Readiness, _ = acct["readiness"].(map[string]any)
	}
	if err != nil {
		return result, fmt.Errorf("account %s was created but this machine could not finish logging in: %w\n  Retry with: tessera login --api-key <API key> \"<recovery phrase>\"", result.AccountID, err)
	}
	return result, nil
}

// createAgentAccount calls POST /agents and, on success, stores the one-time
// key in cfg and saves it. On a save failure the created key is returned
// alongside the error, because the server shows it exactly once.
func createAgentAccount(api billingAPI, cfg *Config, name, contactEmail string) (signupResult, error) {
	accountID, apiKey, err := api.CreateAgent(name, contactEmail)
	if err != nil {
		return signupResult{}, err
	}
	result := signupResult{AccountID: accountID, APIKey: apiKey}
	cfg.BillingAgentKey = apiKey
	if err := saveConfig(cfg); err != nil {
		return result, fmt.Errorf("save config: %w", err)
	}
	return result, nil
}

// printSignupResult prints the account id, App ID and the two secrets to keep.
// This is the documented exception to the never-print-secrets rule.
func printSignupResult(result signupResult, asJSON bool) {
	if asJSON {
		out := map[string]any{
			"account_id":      result.AccountID,
			"api_key":         result.APIKey,
			"recovery_phrase": result.Phrase,
			"app_id":          result.AppID,
			"logged_in":       result.AppID != "",
		}
		if result.Readiness != nil {
			out["readiness"] = result.Readiness
		}
		emitJSON(out)
		return
	}
	bar := strings.Repeat("═", 64)
	fmt.Println()
	fmt.Println(bar)
	fmt.Println("  ACCOUNT CREATED — KEEP THESE TWO THINGS SAFE")
	fmt.Println("  They will not be shown again. Tessera does not store your phrase.")
	fmt.Println(bar)
	if result.Phrase != "" {
		fmt.Println("  1. Recovery phrase (12 words) — restores your account and files:")
		fmt.Printf("       %s\n", result.Phrase)
	}
	fmt.Println("  2. API key — your account password for scripts and support:")
	fmt.Printf("       %s\n", result.APIKey)
	fmt.Println(bar)
	fmt.Printf("  Account ID: %s\n", result.AccountID)
	if result.AppID != "" {
		fmt.Printf("  App ID:     %s\n", result.AppID)
		fmt.Println("  This machine is logged in — no 'tessera login' needed.")
	}
	fmt.Println(bar)
	fmt.Println()
	fmt.Println("Lost this machine? Log back in anywhere with:")
	fmt.Println("  tessera login \"<your 12-word recovery phrase>\"")
	if result.Readiness != nil {
		state, _ := result.Readiness["state"].(string)
		msg, _ := result.Readiness["message"].(string)
		fmt.Println()
		fmt.Printf("Status: %s\n  %s\n", readinessLabel(state), msg)
	}
	fmt.Println()
	fmt.Println("Next: tessera upload <file>. Add funds any time: tessera topup 5 (crypto minimum ~$0.38 in USDC on Base, more for other coins; live list: tessera topup --minimums).")
}
