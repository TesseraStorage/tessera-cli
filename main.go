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

	// "account delete --help" has its own text; only bare "account --help" here.
	if (hasFlag(args, "--help") || hasFlag(args, "-h")) && !(cmd == "account" && len(positional(args)) > 0) && printCommandHelp(cmd) {
		return
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
			usageError("delete <path> [--purge] [--yes]")
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

// commandHelp is the --help text for file commands that have no flag parser
// of their own. Printed to stdout with exit status 0.
var commandHelp = map[string]string{
	"upload": `Usage: {exe} upload <path> [--as <remote-path>] [--quiet]

Upload a file or a folder (folders are uploaded recursively).

  --as <remote-path>  Store it under this remote name (folders: remote prefix;
                      default for a folder: tessera/<name>)
  --quiet             Hide live progress (or set TESSERA_QUIET=1)

Env: TESSERA_SKIP_PREFLIGHT=1 skips the host readiness check.
`,
	"download": `Usage: {exe} download <path> [--out <file>] [--quiet]

Download a stored file and verify its SHA-256.

  --out <file>  Local file name (default: the remote file's base name)
  --quiet       Hide live progress (or set TESSERA_QUIET=1)
`,
	"delete": `Usage: {exe} delete <path> [--purge] [--yes]

Move a file to the trash (restore with 'tessera trash restore').

  --purge     Delete permanently (with its retained versions) instead of trashing
  --yes, -y   Do not ask for confirmation
`,
	"share": `Usage: {exe} share <path>

Create a share link for a stored file. Anyone with the link can download it
with 'tessera fetch <url>'.
`,
	"fetch": `Usage: {exe} fetch <share-url> [output-name] [--quiet]

Download a file from a share link.
`,
	"account": `Usage: {exe} account [--json]
       {exe} account delete [--yes] [--json]   (see 'account delete --help')

Show the account ID, balance, readiness and usage.
`,
	"list": `Usage: {exe} list [prefix] [--json]

List stored files (path, size, date), optionally under a remote prefix.
`,
	"status": `Usage: {exe} status [--json]

Account readiness and total storage used.
`,
	"whoami": `Usage: {exe} whoami

Show this machine's indexer, App ID, the public key of its app key (the
private app key stays in ~/.tessera/config.json as "app_key" and is never
printed), and whether an encrypted copy of the recovery phrase is saved here
(only if you chose one, with a phrase password, at an interactive
'tessera login'; see 'tessera phrase show').
`,
	"usage": `Usage: {exe} usage [--by-folder] [--cost] [--json]

Storage used. --by-folder splits it per top-level folder; --cost adds the
netted egress estimate.
`,
	"versions": `Usage: {exe} versions <path> [--restore <n>] [--json]

List retained versions of a file, or restore one.
`,
	"audit": `Usage: {exe} audit [path] [--verify] [--limit <n>] [--json]

Check stored objects for redundancy; --verify also downloads and checks them.
`,
	"find": `Usage: {exe} find <pattern> [--content] [--json]

Search file names; --content searches inside text files.
`,
	"trash": `Usage: {exe} trash list|restore <path>|rm <path>|empty [--yes]

Inspect, restore or permanently remove trashed files.
`,
}

func init() {
	commandHelp["put"] = commandHelp["upload"]
	commandHelp["get"] = commandHelp["download"]
	commandHelp["rm"] = commandHelp["delete"]
	commandHelp["ls"] = commandHelp["list"]
	commandHelp["du"] = commandHelp["usage"]
	commandHelp["search"] = commandHelp["find"]
}

// printCommandHelp prints cmd's help when it has an entry in commandHelp.
func printCommandHelp(cmd string) bool {
	h, ok := commandHelp[cmd]
	if !ok {
		return false
	}
	fmt.Print(strings.ReplaceAll(h, "{exe}", filepath.Base(os.Args[0])))
	return true
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
                             (shows your recovery phrase + API key once: keep both;
                             when output is not a terminal they go to a private
                             0600 file instead, see 'tessera signup --help')
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
  {exe} upload <path> [--as <remote-path>] [--quiet]
                             Upload a file or folder (--as: name it remotely)
  {exe} download <path> [--out <file>] [--quiet]
                             Download a file (--out: local file name)
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
  {exe} phrase show             Reveal the phrase, if you saved an encrypted copy at an
                             interactive 'tessera login' (locked with a local phrase
                             password; signup never saves one)

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

Account readiness (shown by signup, login, status, account and usage):
  free trial         new accounts get a one-time $0.05 credit (added within
                     about a minute of signup) and Tessera pays the storage
                     hosts; uploads are allowed while the credit lasts
  top up to continue the trial credit is used or the balance is $0: tessera topup 5
  ready              funded; uploads and downloads run normally
Other commands mention readiness only when an upload cannot run.
The low-level "host funding ready" flag stays false until the first top-up,
because during the trial Tessera pays the hosts per upload; that is expected.
Uploads and downloads check host funding first, wait while it lands, and say
what is missing if they cannot run. TESSERA_SKIP_PREFLIGHT=1 skips that check.

Indexer: https://index.tessera.storage
Docs:    https://tessera.storage
`
