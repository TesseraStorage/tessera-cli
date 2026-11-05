package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdSecrets handles `tessera secrets done` (TT-nov5 L): after the user
// confirms the recovery phrase and API key are saved elsewhere, delete the
// plaintext signup-secrets file left by a non-TTY signup.
func cmdSecrets(args []string) {
	if len(args) == 0 || hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println("Usage: tessera secrets done [--yes]")
		fmt.Println()
		fmt.Println("Deletes the plaintext signup-secrets file under ~/.tessera once you have")
		fmt.Println("copied the recovery phrase and API key into a password manager.")
		fmt.Println("The API key in ~/.tessera/config.json is kept (needed to stay logged in).")
		fmt.Println("An encrypted local phrase copy (from login) is also kept, if you made one.")
		return
	}
	if args[0] != "done" {
		fatal("unknown secrets subcommand %q (try: tessera secrets done)", args[0])
	}
	paths := findSignupSecretsFiles()
	if len(paths) == 0 {
		fmt.Println("No signup-secrets file found under ~/.tessera — nothing to delete.")
		return
	}
	fmt.Println("About to delete:")
	for _, p := range paths {
		fmt.Println("  " + p)
	}
	fmt.Println("Only do this after the recovery phrase and API key are safe elsewhere.")
	if !hasFlag(args, "--yes") && !hasFlag(args, "-y") {
		fmt.Print("Delete now? [y/N] ")
		var ans string
		fmt.Scanln(&ans)
		if strings.ToLower(strings.TrimSpace(ans)) != "y" && strings.ToLower(strings.TrimSpace(ans)) != "yes" {
			fmt.Println("Cancelled; secrets file left in place.")
			return
		}
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not delete %s: %v\n", p, err)
		} else {
			fmt.Println("Deleted " + p)
		}
	}
	if cfg, err := loadConfig(); err == nil && cfg.SignupSecretsFile != "" {
		cfg.SignupSecretsFile = ""
		_ = saveConfig(cfg)
	}
}

func findSignupSecretsFiles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	// Honor a custom --secrets-file path recorded in config (TT run 5 #11).
	if cfg, err := loadConfig(); err == nil && cfg.SignupSecretsFile != "" {
		add(cfg.SignupSecretsFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	dir := filepath.Join(home, ".tessera")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if name == "signup-secrets.txt" || (strings.HasPrefix(name, "signup-secrets-") && strings.HasSuffix(name, ".txt")) {
			add(filepath.Join(dir, name))
		}
	}
	return out
}

// secretsFileReminder returns a one-line status reminder when a plaintext
// signup-secrets file still exists.
func secretsFileReminder() string {
	paths := findSignupSecretsFiles()
	if len(paths) == 0 {
		return ""
	}
	return "Secrets file still present (" + paths[0] + "). After the phrase is saved elsewhere, run: tessera secrets done"
}


// secretsFileHint is a one-line prompt after a successful upload/download when
// a plaintext signup-secrets file is still on disk (TT run 5 #11).
func secretsFileHint() string {
	return secretsFileReminder()
}
