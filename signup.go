package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	siastorage "go.sia.tech/siastorage"
	"golang.org/x/term"
)

// stdoutIsTerminal is a var so tests can pin it.
var stdoutIsTerminal = func() bool { return term.IsTerminal(int(syscall.Stdout)) }

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
	Account   map[string]any
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
		fmt.Println("Usage: tessera signup [--name <name>] [--contact-email <email>] [--force]")
		fmt.Println("                      [--json] [--secrets-file <path>] [--show-secrets]")
		fmt.Println()
		fmt.Println("Creates a new account and logs this machine in (no separate 'tessera login').")
		fmt.Println("Gives you your 12-word recovery phrase and API key ONCE — keep both safe.")
		fmt.Println("The phrase alone restores the account later: tessera login \"<12 words>\"")
		fmt.Println()
		fmt.Println("Where the two secrets go:")
		fmt.Println("  terminal            printed on screen")
		fmt.Println("  not a terminal      (a pipe, log or agent) written to a private 0600 file,")
		fmt.Println("                      ~/.tessera/signup-secrets-<account>.txt, and only its")
		fmt.Println("                      path is printed. Copy both into your password manager,")
		fmt.Println("                      then delete the file.")
		fmt.Println("  --secrets-file <p>  always write them to <p> (0600) instead of printing")
		fmt.Println("  --show-secrets      print them even when output is not a terminal")
		fmt.Println("  --json              api_key and recovery_phrase are JSON fields; with")
		fmt.Println("                      --secrets-file they are left out and secrets_file is set")
		fmt.Println("The API key is also saved in ~/.tessera/config.json for this machine; the")
		fmt.Println("phrase is not (only the secrets file holds it, until you delete it).")
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
	printSignupResultTo(result, signupOutputMode(args, asJSON))
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
	if err == nil {
		signupWarmHosts(cfg)
	}
	if acct, aerr := newBillingClient(cfg).Account(); aerr == nil {
		result.Readiness, _ = acct["readiness"].(map[string]any)
		result.Account = acct
	}
	if err != nil {
		return result, fmt.Errorf("account %s was created but this machine could not finish logging in: %w\n  Retry with: tessera login --api-key <API key> \"<recovery phrase>\"", result.AccountID, err)
	}
	return result, nil
}

// warmHostFunding fetches the host list once, best effort. The indexer
// starts funding the new account's host accounts (and applies the free-trial
// credit) on its first host-list request, so doing it here means the balance
// shown by signup and 'tessera account' is already the real one and the
// first upload does not wait for funding from zero.
var signupWarmHosts = warmHostFunding

func warmHostFunding(cfg *Config) {
	client, appKey, err := connectAPI(cfg)
	if err != nil || len(appKey) != 64 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = client.Hosts(ctx, appKey)
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

// signupOutput says where the one-time secrets go (TT retest 3 #8).
type signupOutput struct {
	JSON        bool
	SecretsFile string // "" = print them
	Explicit    bool   // --secrets-file given
}

// signupOutputMode: a terminal prints the secrets; anything else (pipe, log,
// agent transcript) gets a private 0600 file unless --show-secrets; an explicit
// --secrets-file always wins. --json keeps the secrets as fields unless
// --secrets-file is given.
func signupOutputMode(args []string, asJSON bool) signupOutput {
	if p, ok := flagValue(args, "--secrets-file"); ok && p != "" {
		return signupOutput{JSON: asJSON, SecretsFile: p, Explicit: true}
	}
	if asJSON || hasFlag(args, "--show-secrets") || stdoutIsTerminal() {
		return signupOutput{JSON: asJSON}
	}
	return signupOutput{SecretsFile: "auto"}
}

// defaultSecretsPath is ~/.tessera/signup-secrets-<account>.txt.
func defaultSecretsPath(accountID string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	name := "signup-secrets.txt"
	if accountID != "" {
		name = "signup-secrets-" + accountID + ".txt"
	}
	return filepath.Join(dir, name), nil
}

// writeSecretsFile writes the two secrets to path with 0600 permissions (the
// directory is created 0700). An existing file is replaced.
func writeSecretsFile(path string, result signupResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Tessera account secrets, written once by 'tessera signup'.\n")
	b.WriteString("# Copy both into a password manager, then delete this file.\n")
	b.WriteString("# Tessera cannot show them again or recover the phrase.\n")
	fmt.Fprintf(&b, "account_id=%s\n", result.AccountID)
	if result.AppID != "" {
		fmt.Fprintf(&b, "app_id=%s\n", result.AppID)
	}
	if result.Phrase != "" {
		fmt.Fprintf(&b, "recovery_phrase=%s\n", result.Phrase)
	}
	fmt.Fprintf(&b, "api_key=%s\n", result.APIKey)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// printSignupResult prints with the default mode for this output (tests).
func printSignupResult(result signupResult, asJSON bool) {
	printSignupResultTo(result, signupOutputMode(nil, asJSON))
}

// printSignupResultTo prints the account id, App ID and the two secrets to
// keep — on screen, or into a private file whose path is printed. This is the
// documented exception to the never-print-secrets rule.
func printSignupResultTo(result signupResult, mode signupOutput) {
	secretsPath := ""
	if mode.SecretsFile != "" {
		p := mode.SecretsFile
		if p == "auto" {
			var err error
			p, err = defaultSecretsPath(result.AccountID)
			if err != nil {
				p = ""
			}
		}
		if p != "" {
			if err := writeSecretsFile(p, result); err == nil {
				secretsPath = p
			} else {
				fmt.Fprintf(os.Stderr, "Warning: could not write the secrets file %s (%v); printing them instead so they are not lost.\n", p, err)
			}
		}
	}
	if mode.JSON {
		out := map[string]any{
			"account_id": result.AccountID,
			"app_id":     result.AppID,
			"logged_in":  result.AppID != "",
		}
		if secretsPath != "" {
			out["secrets_file"] = secretsPath
		} else {
			out["api_key"] = result.APIKey
			out["recovery_phrase"] = result.Phrase
		}
		if result.Readiness != nil {
			out["readiness"] = result.Readiness
		}
		if v := buildTrialView(result.Account); v != nil {
			out["trial"] = v
		}
		emitJSON(out)
		return
	}
	bar := strings.Repeat("═", 64)
	fmt.Println()
	fmt.Println(bar)
	fmt.Println("  ACCOUNT CREATED — KEEP THESE TWO THINGS SAFE")
	if secretsPath != "" {
		fmt.Println("  Your recovery phrase and API key were NOT printed (this output is")
		fmt.Println("  not a terminal). They are in a private file readable only by you:")
		fmt.Printf("       %s\n", secretsPath)
		fmt.Println("  Copy both into a password manager now, then delete that file:")
		fmt.Printf("       rm %s\n", shellQuote(secretsPath))
		fmt.Println("  Tessera's servers never see your phrase and cannot show either again.")
	} else {
		fmt.Println("  They will not be shown again. Tessera's servers never see your phrase,")
		fmt.Println("  and this machine does not save it: write it down now.")
		fmt.Println(bar)
		if result.Phrase != "" {
			fmt.Println("  1. Recovery phrase (12 words) — restores your account and files:")
			fmt.Printf("       %s\n", result.Phrase)
		}
		fmt.Println("  2. API key — your account password for scripts and support:")
		fmt.Printf("       %s\n", result.APIKey)
	}
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
	v := buildTrialView(result.Account)
	if v == nil && result.Readiness != nil {
		v = buildTrialView(map[string]any{"readiness": result.Readiness})
	}
	if v != nil {
		fmt.Println()
		fmt.Printf("Balance: $%s\n", v.BalanceUSD)
		printTrialBlock(v, "", 8)
	}
	fmt.Println()
	if secretsPath == "" && !stdoutIsTerminal() {
		fmt.Fprintln(os.Stderr, "Note: the recovery phrase and API key above were written to a non-terminal output (a log or pipe). Keep that output private, or delete it after saving both secrets.")
	}
	fmt.Println("Next: tessera upload <file>. Add funds any time: tessera topup 5 (crypto minimum ~$0.38 in USDC on Base, more for other coins; live list: tessera topup --minimums).")
}

// shellQuote single-quotes a path for a copy-paste command.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"\\$`!*?&;|<>()") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
