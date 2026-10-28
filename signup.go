package main

import (
	"errors"
	"fmt"
	"os"
)

// newBillingClient builds the moneyd client a command talks to. It is a
// package-level var (precedent: serviceCommand) so command tests can inject an
// in-memory fake and stay network-free.
var newBillingClient = func(cfg *Config) billingAPI {
	return newBillingAPI(cfg)
}

// signupResult is the server's one-time response to an agent signup.
type signupResult struct {
	AccountID string
	APIKey    string
}

// errSignupKeyExists stops a second signup from silently replacing the only
// locally stored agent key; --force opts in to a new account.
var errSignupKeyExists = errors.New("already signed up on this machine; pass --force to create a new agent account")

// cmdSignup creates a new agent account and stores its one-time API key.
//
//	tessera signup [--name <name>] [--contact-email <email>] [--force] [--json]
//
// The API key is the account's only credential and moneyd hands it out once,
// so this is the one command that deliberately prints a secret: exactly once,
// with a store-it warning. Every later command keeps it inside the 0600
// config. Signup is unauthenticated and needs no config file, so it is also
// the cold-start path; no storage credentials are minted here (registration
// happens after login).
func cmdSignup(args []string) {
	// Side-effect free out: cmdSignup never checked --help/-h, so
	// `tessera signup --help` created a real agent account and printed a
	// real one-time API key. Fixed per the same login.go --help bug.
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println("Usage: tessera signup [--name <name>] [--contact-email <email>] [--force] [--json]")
		fmt.Println()
		fmt.Println("Creates a new agent account and stores its one-time API key.")
		fmt.Println("This command does not run when only --help/-h is given.")
		return
	}
	result, err := signup(args)
	if err != nil && result.APIKey == "" {
		fatal("%v", err)
	}
	// Print exactly once: on success, and also when the account was created
	// but the local save failed — the server will never show the key again,
	// so losing it to a disk error would strand the account.
	printSignupResult(result, hasFlag(args, "--json"))
	if err != nil {
		fatal("%v", err)
	}
}

// signup does the command's real work without printing or exiting: resolve the
// flags, cold-start the config when none exists, create the agent, persist the
// key. Split out so tests can exercise the failure paths without os.Exit.
func signup(args []string) (signupResult, error) {
	name, _ := flagValue(args, "--name")
	contactEmail, _ := flagValue(args, "--contact-email")

	cfg, err := loadConfig()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A config that exists but cannot be read (corrupt, unsupported
			// indexer) must not be silently replaced by a fresh one.
			return signupResult{}, fmt.Errorf("read config: %w", err)
		}
		// Cold start: the first thing a new user runs must not require a
		// config file. saveConfig fills in the URLs too, but setting them
		// here keeps the in-memory copy the client sees consistent.
		cfg = &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL}
	}
	if cfg.BillingAgentKey != "" && !hasFlag(args, "--force") {
		// Overwriting the stored key would strand the account it belongs to:
		// moneyd shows the key once, so a fresh signup loses the old one.
		return signupResult{}, errSignupKeyExists
	}
	return createAgentAccount(newBillingClient(cfg), cfg, name, contactEmail)
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

// printSignupResult prints the account id and the one-time key. This is the
// documented exception to the never-print-secrets rule; the key appears
// exactly once per signup and no other command ever echoes it.
func printSignupResult(result signupResult, asJSON bool) {
	if asJSON {
		emitJSON(map[string]string{
			"account_id": result.AccountID,
			"api_key":    result.APIKey,
		})
		return
	}
	fmt.Println("Agent account created.")
	fmt.Println()
	fmt.Printf("  account id:  %s\n", result.AccountID)
	fmt.Printf("  API key:     %s\n", result.APIKey)
	fmt.Println()
	fmt.Println("IMPORTANT: store this API key now. It will not be shown again;")
	fmt.Println("it is saved in your local config file (keep that file private).")
}
