// Tessera CLI — command-line interface for Tessera decentralized storage.
//
// Commands:
//
//	tessera login       Connect this machine to Tessera
//	tessera signup      Create an agent account (API key shown once)
//	tessera account     Show billing account, balance, and usage
//	tessera topup       Add funds (NOWPayments invoice or x402 USDC)
//	tessera promo       Redeem a promo code
//	tessera logout      Remove local credentials
//	tessera whoami      Show current account identity
//	tessera status      Account info + total storage used
//	tessera list        List all files (path, size, date)
//	tessera upload      Upload a file or folder
//	tessera download    Download a file
//	tessera delete      Delete a file
//	tessera share       Create a share link for a file
//	tessera fetch       Download a shared file
//	tessera sync        Sync a local folder with the network
//	tessera folder      Manage the drop-in Tessera folder
//	tessera find        Search your files
//	tessera versions    List or restore retained versions
//	tessera trash       Soft delete, restore, empty
//	tessera audit       Verify durability
//	tessera usage       Storage usage and cost forecast
//	tessera index       Rebuild the local index
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// statusCommand is the `status` dispatch seam (precedent: serviceCommand):
// main passes the command arguments through it so `status --json` reaches
// cmdStatus's JSON branch. Tests replace it to observe the arguments without
// cmdStatus's network calls.
var statusCommand = cmdStatus

// version is the build version. CI injects it from the release tag so it can
// never drift from the release:
//
//	go build -ldflags "-X main.version=${GITHUB_REF_NAME}" .
//
// It stays "dev" for local and non-tag builds.
var version = "dev"

// cmdVersion prints this build's version. The value is injected at release
// time from the git tag (see the `version` var), so it always matches the tag.
// It also reports a newer release when one is known (best-effort, throttled).
func cmdVersion() {
	fmt.Println("tessera " + version)
	if cfg, err := loadConfig(); err == nil {
		if msg := updateNotice(cfg); msg != "" {
			fmt.Println(msg)
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "version", "--version":
		cmdVersion()
	case "login":
		cmdLogin(args)
	case "signup":
		cmdSignup(args)
	case "account":
		cmdAccount(args)
	case "topup":
		if !hasFlag(args, "--coins") && len(positional(args)) < 1 {
			usageError("topup <amount> [--coin <code>] [--invoice] [--wallet] [--json] [--wait|--no-wait]")
		}
		cmdTopup(args)
	case "wallet":
		if err := runWallet(args); err != nil {
			fatal("%v", err)
		}
	case "promo":
		if len(positional(args)) < 1 {
			usageError("promo <code>")
		}
		cmdPromo(args)
	case "logout":
		cmdLogout()
	case "whoami":
		cmdWhoami()
	case "status":
		statusCommand(args...)
	case "list", "ls":
		cmdList(args)
	case "upload", "put":
		if len(positional(args)) < 1 {
			usageError("upload <path> [--as <remote-path>]")
		}
		cmdUploadPath(positional(args)[0], args)
	case "download", "get":
		if len(positional(args)) < 1 {
			usageError("download <path> [--out <file>]")
		}
		cmdDownload(args)
	case "delete", "rm":
		if len(positional(args)) < 1 {
			usageError("delete <path> [--versions]")
		}
		cmdDelete(args)
	case "share":
		if len(positional(args)) < 1 {
			usageError("share <path>")
		}
		cmdShare(positional(args)[0])
	case "fetch":
		if len(positional(args)) < 1 {
			usageError("fetch <share-url> [output-name]")
		}
		pos := positional(args)
		outName := ""
		if len(pos) >= 2 {
			outName = pos[1]
		}
		cmdFetch(pos[0], outName)
	case "sync":
		cmdSync(args)
	case "folder":
		cmdFolder(args)
	case "find", "search":
		cmdFind(args)
	case "versions":
		cmdVersions(args)
	case "trash":
		cmdTrash(args)
	case "audit":
		cmdAudit(args)
	case "usage", "du":
		cmdUsage(args)
	case "index":
		cmdIndex(args)
	case "config":
		cmdConfig(args)
	case "service":
		cmdService(args)
	case "uninstall":
		cmdUninstall(args)
	case "phrase":
		cmdPhrase(args)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}

	// Quiet, throttled "newer version" notice (Packet 4). Best-effort: any
	// failure is silent, it is opt-out via 'tessera config --update-check off',
	// and it never installs anything. cmdVersion prints it itself.
	if cmd != "version" && cmd != "--version" {
		if cfg, err := loadConfig(); err == nil {
			if msg := updateNotice(cfg); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
		}
	}
}

// usageError prints a one-line usage hint and exits.
func usageError(usage string) {
	fmt.Fprintf(os.Stderr, "Usage: %s %s\n", filepath.Base(os.Args[0]), usage)
	os.Exit(1)
}

func printUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Printf(`Tessera CLI — decentralized storage on Sia.

Usage:
  %s login                   Connect this machine to Tessera
  %s logout                  Remove local credentials
  %s whoami                  Show current account identity
  %s status                  Account info + total storage used
  %s list [prefix]           List files (path, size, date)
  %s upload <path>           Upload a file or folder
  %s download <path>         Download a file
  %s delete <path>           Delete a file (see also: trash)
  %s share <path>            Create a share link for a file
  %s fetch <url> [name]      Download a shared file

Folders & sync:
  %s sync add <folder> --as <remote-prefix>
                             Start syncing a folder (see 'sync add -h')
  %s sync [--dry-run]        Sync registered folders once
  %s sync watch              Keep folders in sync continuously
  %s sync status|list|remove|conflicts
  %s folder add <path>       Create a drop-in folder and start syncing
  %s folder open|status      Open it in your file manager

More:
  %s find <pattern>          Search your files (--content for text search)
  %s versions <path>         List retained versions
  %s trash list|restore|empty|rm <path>
  %s audit [path]            Verify durability of stored data
  %s usage [--by-folder] [--cost] [--json]
                             Storage usage; --cost adds the netted egress estimate
  %s index --rebuild         Rebuild the local metadata index
  %s config [--json]         Show or change settings
  %s version                 Print this build's version (from the release tag)
  %s service install|start|stop|status
                             Run the watcher as a background service
  %s uninstall               Remove the watcher, this binary, and (optionally) local state
  %s phrase show             Reveal the locally-saved recovery phrase (master password)

Billing:
  %s account [--json]        Show billing account, balance, and usage
  %s topup <amount> [--coin <code>] [--invoice] [--wallet]
                             Add funds by address (default), invoice, or x402 wallet
  %s topup <amount> [--json] [--wait|--no-wait]
                             Machine-readable output; poll until a terminal state
  %s topup <amount> --payment <base64 X-PAYMENT>
                             Submit a signed x402 authorization to settle
  %s topup --coins           List coins accepted for top-ups
  %s wallet init|show|fund   Local x402 wallet (opt-in)
  %s signup [--name <name>] [--contact-email <email>] [--force] [--json]
                             Create an agent account; the API key is shown once
  %s login --email <email> [--token <token>]
                             Sign in with a magic link (billing session)
  %s login --billing-logout  End the billing session (agent key untouched)
  %s promo <code>            Redeem a promo code

New accounts get a free first upload; the minimum top-up is $0.01.
Uploads and downloads first check that enough storage hosts are funded and
reachable (TCP, usually port 9988), wait while funding lands, and say what is
missing if they cannot run. TESSERA_SKIP_PREFLIGHT=1 skips that check.

Indexer: https://index.tessera.storage
Docs:    https://tessera.storage
`, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe,
		exe, exe, exe, exe, exe, exe,
		exe, exe, exe, exe, exe, exe,
		exe, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe)
}
