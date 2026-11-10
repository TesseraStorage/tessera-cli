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
// Signup always starts from a clean config, creates the storage identity in
// the same step and registers the seed-login key, then prints the two things
// to keep: the recovery phrase and the API key. This is the one command that
// deliberately prints secrets, exactly once.
func cmdSignup(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println("Usage: tessera signup [--name <name>] [--contact-email <email>] [--force]")
		fmt.Println("                      [--json] [--secrets-file <path>] [--show-secrets]")
		fmt.Println()
		fmt.Println("Creates a new account and logs this machine in (no separate 'tessera login').")
		fmt.Println("Gives you your 12-word recovery phrase and API key ONCE — keep both safe.")
		fmt.Println("The phrase alone restores the account later: tessera login \"<12 words>\"")
		fmt.Println()
		fmt.Println("Options:")
		fmt.Println("  --name <name>           display name for the account (optional)")
		fmt.Println("  --contact-email <email> optional. Stored so Tessera can email billing")
		fmt.Println("                          notices (low balance, suspension) for this")
		fmt.Println("                          account. After you confirm it (from the dashboard")
		fmt.Println("                          or a confirmation email), it can also be used to")
		fmt.Println("                          sign in with a magic link. Not required for")
		fmt.Println("                          signup, API-key login, or storage.")
		fmt.Println()
		fmt.Println("Where the two secrets go:")
		fmt.Println("  terminal            printed on screen")
		fmt.Println("  not a terminal      (a pipe, log or agent) written to a private 0600 file,")
		fmt.Println("                      ~/.tessera/signup-secrets-<account>.txt, and only its")
		fmt.Println("                      path is printed. Copy both into your password manager,")
		fmt.Println("                      then delete the file (or run: tessera secrets done).")
		fmt.Println("  --secrets-file <p>  always write them to <p> (0600) instead of printing")
		fmt.Println("  --show-secrets      print them even when output is not a terminal")
		fmt.Println("  --json              api_key and recovery_phrase are JSON fields; with")
		fmt.Println("                      --secrets-file they are left out and secrets_file is set")
		fmt.Println("The API key is also saved in ~/.tessera/config.json for this machine; the")
		fmt.Println("phrase is not saved there. Login may offer to store an encrypted local copy")
		fmt.Println("of the phrase (optional, locked with a phrase password you choose).")
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

// warmHostFunding fetches the host list once so the indexer claims the
// one-time trial host set and enqueues funding, then waits until enough
// hosts are funded for a trial upload (30 of 30+spares). Signup used to
// return as soon as the list was fetched, so the first upload still waited
// ~20s for the last of 30 hosts (TT run 5 #7).
var signupWarmHosts = warmHostFunding

// signupFundingNeeded is how many funded GoodForUpload hosts a trial upload
// needs (10 data + 20 parity). Spares are funded too (trial bag is ~40).
const signupFundingNeeded = dataShards + parityShards

// signupFundingWait is the max time signup waits for host funding after the
// warm host-list call. Funding is async (hostfilter fundWorkers); most
// accounts are ready well under a minute.
const signupFundingWait = 90 * time.Second

func warmHostFunding(cfg *Config) {
	client, appKey, err := connectAPI(cfg)
	if err != nil || len(appKey) != 64 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = client.Hosts(ctx, appKey)

	// Wait until enough hosts show a funded balance so the first upload does
	// not stall on "waiting for host funding: 29/30".
	waitCtx, waitCancel := context.WithTimeout(context.Background(), signupFundingWait)
	defer waitCancel()
	d, derr := newReadinessDeps(cfg)
	if derr != nil {
		return
	}
	d.progress = func(funded, needed int) {
		fmt.Fprintf(os.Stderr, "Host funding: %d/%d hosts funded (Tessera prepays the hosts at signup)...\n", funded, needed)
	}
	if err := checkReadiness(waitCtx, d, signupFundingNeeded, true); err != nil {
		fmt.Fprintf(os.Stderr, "Note: host funding is still catching up (%v). The first upload will wait for the rest.\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "Host funding ready for the technical-validation upload.")
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
				// Remember the path (including custom --secrets-file) so
				// `tessera secrets done` can delete it later.
				if cfg, err := loadConfig(); err == nil {
					cfg.SignupSecretsFile = p
					_ = saveConfig(cfg)
				}
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
			out["readiness"] = publicReadinessJSON(result.Readiness, buildTrialView(map[string]any{"readiness": result.Readiness}))
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
		fmt.Println("  They will not be shown again. Tessera's servers never see your phrase.")
		fmt.Println("  This machine does not save the phrase unless you later choose an encrypted")
		fmt.Println("  local copy at login (optional). Write it down now.")
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
	fmt.Println("Next: tessera upload <file>. Add funds any time: tessera topup 1 (crypto minimum ~$0.38 in USDC on Base, more for other coins; live list: tessera topup --minimums).")
	if secretsPath != "" {
		fmt.Println("When the phrase and API key are safe in your password manager, delete the secrets file:")
		fmt.Println("  tessera secrets done")
	}
}

// shellQuote single-quotes a path for a copy-paste command.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"\\$`!*?&;|<>()") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
