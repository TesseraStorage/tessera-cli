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
	"strings"
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

	// Daily update check, started in the background so it never delays the
	// command; its result is printed (at most) once the command finishes.
	notices := startBackgroundNotices(cmd, args)
	switch cmd {
	case "upload", "put", "download", "get", "fetch":
		configureProgress(args)
	}

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
		if !hasFlag(args, "--coins") && !hasFlag(args, "--minimums") && !hasFlag(args, "--help") && !hasFlag(args, "-h") && len(positional(args)) < 1 {
			usageError("topup <amount> [--asset <code>] [--invoice] [--wallet] [--json] [--wait|--no-wait]  (tessera topup --help)")
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
		cmdLogout(args...)
	case "update", "upgrade":
		cmdUpdate(args)
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

	notices.print()
}

// usageError prints a one-line usage hint and exits.
func usageError(usage string) {
	fmt.Fprintf(os.Stderr, "Usage: %s %s\n", filepath.Base(os.Args[0]), usage)
	os.Exit(1)
}

func printUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Print(strings.ReplaceAll(usageText, "{exe}", exe))
}

const usageText = `Tessera CLI — decentralized storage on Sia.

Get started:
  {exe} signup                  Create an account AND log this machine in
                             (prints your recovery phrase + API key once: keep both)
  {exe} login "<12 words>"      Restore your account on any machine (same account + App ID)
  {exe} login --api-key <key> ["<12 words>"]
                             Log in with your API key
  {exe} login --email <email> [--token <token>]
                             Sign in by email (magic link)
  {exe} logout [--yes]          Remove local credentials (account and files stay)

Files:
  {exe} whoami                  Show current account identity
  {exe} status                  Account readiness + total storage used
  {exe} list [prefix]           List files (path, size, date)
  {exe} upload <path> [--quiet] Upload a file or folder
  {exe} download <path> [--quiet]
                             Download a file
                             (live progress on stderr: phase, bytes, %, speed, ETA;
                             plain lines every 10s when stderr is not a terminal;
                             --quiet / TESSERA_QUIET=1 hides it)
  {exe} delete <path>           Delete a file (see also: trash)
  {exe} share <path>            Create a share link for a file
  {exe} fetch <url> [name]      Download a shared file

Folders & sync:
  {exe} sync add <folder> --as <remote-prefix>
                             Start syncing a folder (see 'sync add -h')
  {exe} sync [--dry-run]        Sync registered folders once
  {exe} sync watch              Keep folders in sync continuously
  {exe} sync status|list|remove|conflicts
  {exe} folder add <path>       Create a drop-in folder and start syncing
  {exe} folder open|status      Open it in your file manager

More:
  {exe} find <pattern>          Search your files (--content for text search)
  {exe} versions <path>         List retained versions
  {exe} trash list|restore|empty|rm <path>
  {exe} audit [path]            Verify durability of stored data
  {exe} usage [--by-folder] [--cost] [--json]
                             Storage usage; --cost adds the netted egress estimate
  {exe} index --rebuild         Rebuild the local metadata index
  {exe} config [--json]         Show or change settings
  {exe} version                 Print this build's version
  {exe} update [--check]        Install the latest release (SHA256SUMS-verified)
  {exe} service install|start|stop|status
                             Run the watcher as a background service
  {exe} uninstall               Remove the watcher, this binary, and (optionally) local state
  {exe} phrase show             Reveal the locally-saved recovery phrase (master password)

Billing & account:
  {exe} account [--json]        Show account, balance, readiness and usage
  {exe} account delete [--yes]  Permanently delete your account and all files
                             (remaining balance is forfeited, no refunds)
  {exe} topup <amount> [--asset <code>] [--invoice] [--wallet]
                             Add funds: x402 wallet (any amount from $0.01),
                             deposit address in a chosen coin (USDC on Base by
                             default; XMR, BTC, ETH, USDT...), or browser invoice.
                             See 'tessera topup --help'.
  {exe} topup <amount> [--json] [--wait|--no-wait]
                             Machine-readable output; poll until a terminal state
  {exe} topup <amount> --payment <base64 X-PAYMENT>
                             Submit a signed x402 authorization to settle
  {exe} topup --coins           List coins accepted for top-ups
  {exe} topup --minimums [--asset <code>]
                             Live minimums: crypto via NOWPayments has a per-coin
                             processor minimum (~$0.38 USDC on Base, more for
                             other coins); x402 (--wallet) from the $0.01 floor
  {exe} wallet init|show|fund   Local x402 wallet (opt-in)
  {exe} login --billing-logout  End the email session (API key untouched)
  {exe} promo <code>            Redeem a promo code

Account readiness (shown by signup, login, status and account):
  free trial         new accounts: the first upload is prepaid by Tessera
  top up to continue the trial is used or the balance is $0: tessera topup 5
  ready              funded; uploads and downloads run normally
The low-level "host funding ready" flag from the indexer stays false for a
free-trial account until its first top-up; that is expected, not an error.
Uploads and downloads check host funding first, wait while it lands, and say
what is missing if they cannot run. TESSERA_SKIP_PREFLIGHT=1 skips that check.

Indexer: https://index.tessera.storage
Docs:    https://tessera.storage
`
