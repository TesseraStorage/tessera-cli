package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go.sia.tech/core/types"
	indexdapp "go.sia.tech/indexd/api/app"
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

// openBrowser tries to open url in the default browser.
func openBrowser(url string) error {
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

func cmdLogin() {
	fmt.Println("Tessera CLI — Login")
	fmt.Println()

	cfg, err := loadConfig()
	if err == nil && cfg.AppKey != "" {
		fmt.Print("Already logged in. Log out first? [y/N] ")
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(a) != "y" {
			fmt.Println("Aborted.")
			return
		}
	}

	appID := siastorage.GenerateAppID()
	indexer := defaultIndexer

	cfg = &Config{
		IndexerURL: indexer,
		AppID:      appID.String(),
	}

	fmt.Printf("Indexer:  %s\n", indexer)
	fmt.Printf("App ID:   %s\n", appID.String())
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Step 1 — request connection
	fmt.Print("Starting connection... ")
	builder := siastorage.NewBuilder(indexer, siastorage.AppMetadata{
		ID:          appID,
		Name:        "Tessera CLI",
		Description: "Tessera command-line client",
		ServiceURL:  indexer,
	})

	respURL, err := builder.RequestConnection(ctx)
	if err != nil {
		fatal("connection request failed: %v", err)
	}
	fmt.Println("done")

	// Step 2 — open browser for the user to approve
	fmt.Println()
	fmt.Println("A browser tab will open. Enter your connect key to approve.")
	fmt.Println()
	fmt.Printf("  %s\n", respURL)
	fmt.Println()

	if err := openBrowser(respURL); err != nil {
		fmt.Println("Could not open browser automatically.")
		fmt.Printf("Please open this URL manually:\n  %s\n\n", respURL)
	}

	// Step 3 — wait for the user to approve in the browser
	fmt.Print("Waiting for your approval... ")
	if err := builder.WaitForApproval(ctx); err != nil {
		fatal("wait failed: %v\n  (did you approve in the browser?)", err)
	}

	// Step 4 — generate recovery phrase and register
	phrase := siastorage.NewSeedPhrase()
	fmt.Println("approved!")
	fmt.Println()
	fmt.Println(strings.Repeat("═", 54))
	fmt.Println("  RECOVERY PHRASE — SAVE THESE 12 WORDS")
	fmt.Println("  They are the ONLY way to recover your account.")
	fmt.Println("  We do not store them.")
	fmt.Println(strings.Repeat("═", 54))
	fmt.Printf("  %s\n", phrase)
	fmt.Println(strings.Repeat("═", 54))
	fmt.Println()

	sdk, err := builder.Register(ctx, phrase)
	if err != nil {
		fatal("registration failed: %v", err)
	}

	appKey := sdk.AppKey()
	cfg.AppKey = hex.EncodeToString([]byte(appKey))
	sdk.Close()

	// Optional: encrypt phrase locally
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

	if err := saveConfig(cfg); err != nil {
		fatal("save config: %v", err)
	}

	fmt.Println()
	fmt.Println("Logged in. Use 'tessera list' to see your files.")
}

func cmdLogout() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Not logged in.")
		return
	}
	fmt.Printf("App ID: %s\n", cfg.AppID)
	fmt.Print("Remove local credentials? [y/N] ")
	var a string
	fmt.Scanln(&a)
	if strings.ToLower(a) != "y" {
		fmt.Println("Aborted.")
		return
	}
	if err := deleteConfigFiles(); err != nil {
		fatal("logout: %v", err)
	}
	fmt.Println("Logged out. Local credentials removed.")
}

func cmdWhoami() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	fmt.Printf("Indexer:  %s\n", cfg.IndexerURL)
	fmt.Printf("App ID:   %s\n", cfg.AppID)
	if len(cfg.AppKey) >= 16 {
		fmt.Printf("App Key:  %s... (seed saved)\n", cfg.AppKey[:16])
	} else {
		fmt.Println("App Key:  (missing)")
	}
	if cfg.PhraseEncrypted != "" {
		fmt.Println("Recovery: encrypted phrase saved (password-protected)")
	} else {
		fmt.Println("Recovery: phrase NOT saved locally")
	}
}

func cmdStatus() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, appKey, err := connectAPI(cfg)
	if err != nil {
		fatal("%v", err)
	}

	acct, err := client.Account(ctx, appKey)
	if err != nil {
		fatal("account: %v", err)
	}
	fmt.Printf("Ready:     %v\n", acct.Ready)

	var total uint64
	var count int
	var cursor slabs.Cursor
	for {
		evs, err := client.ListObjects(ctx, appKey, cursor, 100)
		if err != nil {
			fatal("list: %v", err)
		}
		if len(evs) == 0 {
			break
		}
		for _, ev := range evs {
			if ev.Deleted {
				continue
			}
			count++
			if ev.Object != nil {
				for _, ss := range ev.Object.Slabs {
					total += uint64(ss.Length)
				}
			}
		}
		last := evs[len(evs)-1]
		next := slabs.Cursor{Key: last.Key, After: last.UpdatedAt}
		if sameCursor(cursor, next) {
			break
		}
		cursor = next
	}
	fmt.Printf("Files:     %d\n", count)
	fmt.Printf("Total:     %s\n", formatBytes(total))
}

func sameCursor(a, b slabs.Cursor) bool {
	return a.Key == b.Key && a.After.Equal(b.After)
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
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprintf(os.Stderr, "ERROR: %s", msg)
	os.Exit(1)
}

// findObject scans the account for an object whose metadata name matches.
func findObject(ctx context.Context, sdk siastorage.SDK, name string) (siastorage.Object, error) {
	var cursor slabs.Cursor
	for {
		evs, err := sdk.ObjectEvents(ctx, cursor, 100)
		if err != nil {
			return siastorage.Object{}, fmt.Errorf("list: %w", err)
		}
		if len(evs) == 0 {
			break
		}
		for _, ev := range evs {
			if ev.Deleted || ev.Object == nil {
				continue
			}
			got := ev.Key.String()[:12] + "..."
			if meta := ev.Object.Metadata(); len(meta) > 0 {
				var m map[string]string
				if json.Unmarshal(meta, &m) == nil {
					if n, ok := m["name"]; ok && n != "" {
						got = n
					}
				}
			}
			if got == name {
				return *ev.Object, nil
			}
		}
		last := evs[len(evs)-1]
		next := slabs.Cursor{Key: last.Key, After: last.UpdatedAt}
		if sameCursor(cursor, next) {
			break
		}
		cursor = next
	}
	return siastorage.Object{}, fmt.Errorf("file not found: %s", name)
}
