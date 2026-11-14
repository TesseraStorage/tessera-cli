# Tessera CLI

Command-line client for [Tessera](https://tessera.storage) — decentralized storage on the Sia network.

**Encrypted. Erasure-coded. 30 hosts. No single point of failure.**

Sync folders across your machines, drag files into a Tessera folder, search,
version and verify everything you store — all from one static binary.

## Quickstart

### macOS / Linux

```bash
curl -fsSL https://tessera.storage/install.sh | sh
# same script: curl -fsSL https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.sh | sh
# a specific version: ... | sh -s v1.14.2
```

Current release: **v1.14.2**. `tessera version` reports the git tag the release
workflow stamps into the binary. The same release also publishes `install.sh`
and `install.ps1` (`https://github.com/TesseraStorage/tessera-cli/releases/latest/download/install.sh`).

The installer verifies the binary against the release's `SHA256SUMS`, never
uses sudo and installs to `~/.local/bin`. If that directory is not on your
`PATH` it adds one marked line to your shell's startup file (`~/.zshrc`,
`~/.bashrc`/`~/.bash_profile`, fish's `config.fish`, else `~/.profile`): it
asks first when it can reach your terminal, and does it automatically, saying
which file it changed, when there is no terminal (agents, CI). Open a new shell
or run `export PATH="$HOME/.local/bin:$PATH"` to use it right away.

Installer environment variables:

| Variable | Effect |
|---|---|
| `TESSERA_INSTALL_DIR` | Install somewhere other than `~/.local/bin` |
| `TESSERA_NO_MODIFY_PATH=1` | Never edit shell startup files (or the Windows user PATH) |
| `TESSERA_RELEASE_REPO=owner/repo` | Download from a fork or mirror instead of `TesseraStorage/tessera-cli`. The installer prints a warning, because the checksum then only proves the file matches *that* repo's release |
| `TESSERA_RELEASE_BASE_URL` | Download the asset and `SHA256SUMS` from this base URL (air-gapped mirrors, tests); also warns |

**Network needs.** Installing needs HTTPS to github.com. Using Tessera needs
HTTPS to `tessera.storage` / `index.tessera.storage` **and outbound TCP to the
storage hosts**, on the port each host announces (usually 9984; some use
others such as 9994 or 9974). A network that allows only HTTP(S) cannot upload
or download.

### Windows (PowerShell)

```powershell
powershell -c "irm https://tessera.storage/install.ps1 | iex"
```

Installs to `%LOCALAPPDATA%\Tessera\bin` and adds it to your user PATH (new
terminals pick it up). The same environment variables apply.

### Manual download

| Platform | Binary |
|---|---|
| **Linux** (x86_64) | `tessera-linux-amd64` |
| **macOS** (Apple Silicon) | `tessera-darwin-arm64` |
| **macOS** (Intel) | `tessera-darwin-amd64` |
| **Windows** (x86_64) | `tessera-windows-amd64.exe` |

### Next steps

```bash
tessera signup                   # Creates your account AND logs you in — save the 12 words + API key
                                 # (piped/agent runs: secrets go to a 0600 file instead of stdout)
tessera upload photo.png         # Erasure-coded across 30 Sia hosts
tessera list                     # See all your files, with real names and paths
tessera download photo.png       # Get it back, SHA-256 verified
tessera share photo.png          # Create a 30-day share link
tessera folder add               # Create a drop-in folder that syncs everywhere
```

## Sync folders across machines

Pair a local folder with a remote prefix. Two machines pointed at the same
prefix converge automatically — files, renames and **deletions**.

```bash
# On the Mac
tessera sync add ~/Documents --as tessera/macos/Documents

# On Windows, into a matching folder
tessera sync add D:\tessera\macos\Documents --as tessera/macos/Documents

# Keep both in sync
tessera sync          # one pass
tessera sync watch    # continuously
```

Deleting a file in one folder deletes it in the other. Creating a file on
Windows downloads it on the Mac. Renaming is a metadata-only operation, so it
does not re-upload content.

### How it decides

Every file carries a content hash, so sync is a three-way comparison between
*what we last agreed on*, *what is on disk now*, and *what the network now has*:

| Situation | Result |
|---|---|
| Local file changed | Uploaded |
| Remote file changed | Downloaded |
| Local file deleted | Deleted remotely (tombstone) |
| Remote file deleted | Deleted locally |
| Both changed | Conflict — see policy below |
| Renamed | Re-pointed, no re-upload |

**Conflict policy.** By default the copy with the newer modification time wins.
Set `--policy keep-both` (or per folder with `sync add --policy keep-both`) to
preserve both versions instead: the loser is kept as
`report (conflicted copy, WINDOWS-PC 2026-09-25-143005).pdf`, visible on every
machine. `tessera sync conflicts` lists them.

**First sync of an existing folder.** The default `--adopt merge` compares per
file: bytes that already match are adopted without transferring anything, and
where they differ the newer copy wins. Use `--adopt local` to push what is
already there, or `--adopt remote` to mirror the network instead.

### Useful flags

```bash
tessera sync --dry-run              # show exactly what would change
tessera sync --json                 # machine-readable summary
tessera sync --max-rate 20MB        # cap bandwidth
tessera sync --jobs 4               # parallel transfers
tessera sync status                 # ahead/behind counts, no transfers
```

### Ignoring files

A `.tesseraignore` at the root of a synced folder uses gitignore-style
patterns. `.DS_Store`, `Thumbs.db`, `desktop.ini`, editor temp files and
partial downloads are always ignored.

```
*.log
build/
!important.log
```

## The drop-in folder

Prefer to just drag files in? `tessera folder add` creates a normal directory,
registers it for sync and opens it in Finder/Explorer/your file manager.

```bash
tessera folder add                          # ~/Tessera by default
tessera folder add ~/Dropbox-ish --as tessera/drop
tessera folder add --install-service        # set up and go live in one step
tessera folder open
tessera folder status
tessera folder watch                        # keep it in sync in this terminal
```

`folder add` syncs the folder **once** and then stops. A folder that is not
watched only syncs when you run `tessera sync`, so it will offer:

```
Keep this folder in sync automatically? [Y/n]
```

Answering yes installs the background watcher for you. If you are piping output
or running non-interactively it cannot ask, so pass `--install-service`, or set
it up afterwards:

```bash
tessera service install             # preview what will be installed
tessera service install --yes       # actually install and start it
tessera service uninstall --yes     # stop and remove it
tessera service status              # installed? running?
```

Without `--yes` nothing is changed — you get the exact commands that would run.
`service print` shows the definitions without writing anything.

Files are downloaded for real (like Dropbox's "available offline" mode) — this
build does not mount a virtual filesystem, which keeps the single static binary
and avoids kernel drivers.

## Syncing a Mac folder to a Windows PC

This is the part that trips people up, so here it is end to end.

**First, both machines must be on the same account.** This is the step that
actually matters:

```bash
# On the Mac: create the account and SAVE THE 12 WORDS (and the API key)
tessera signup

# On Windows: log in with that SAME recovery phrase — no browser, no password
tessera login "word1 word2 ... word12"
```

The phrase restores the same account and the same App ID, so both machines see
the same files. Running `signup` on the second machine would create a *separate
account* instead. (You can also set `TESSERA_PHRASE`.)

**Then pair the folders using the same remote prefix:**

```bash
# Mac — the folder you want to share
tessera folder add ~/Documents --as tessera/macos/Documents --install-service

# Windows — where it should land locally
tessera folder add C:\tessera\macos\Documents --as tessera/macos/Documents --install-service
```

The local paths differ (that is expected); the **remote prefix must match
exactly**. Paths are always stored with `/` internally, so a folder created on
Windows is addressable from macOS and vice versa.

Check it worked:

```bash
tessera status          # Files: N — should match on both
tessera sync status     # to upload / to fetch should both be 0
```

To verify the pairing before trusting it, add a file on the Mac and run
`tessera sync` on Windows — it should appear.

**The reverse direction works the same way.** To also push Windows documents to
the Mac, add a second pair with a different prefix (for example
`tessera/windows/Documents`) on both machines.

> Windows paths in Git Bash or PowerShell: quote them if they contain spaces,
> and prefer `C:\tessera\...` over a OneDrive-synced folder — two sync clients
> watching one directory will fight over it.

## Commands

| Command | Description |
|---|---|
| `login "<12 words>"` | Restore your account on this machine from the recovery phrase: same account ID and App ID, no browser |
| `login --api-key <tsk_…> ["<12 words>"]` | Log in with your API key (adding the phrase restores the storage identity too) |
| `login --code <ABCD-1234> ["<12 words>"]` | Connect this machine with a one-time code from `/account`. A new account shows a recovery phrase once; an account that already has a storage identity asks for the phrase from the first device |
| `login --email <e> [--token <t>]` | Request or finish an emailed magic link (still works) |
| `login --billing-logout` | End the billing session (an API key, if any, is untouched) |
| `signup [--name <n>] [--contact-email <e>] [--json] [--force] [--secrets-file <path>] [--show-secrets]` | Create an account and log this machine in; the recovery phrase and API key are shown once — keep both. When stdout is not a terminal they go to a `0600` file instead (see below) |
| `logout [--yes]` | Remove local credentials from `~/.tessera/` (account and files stay) |
| `account delete [--yes]` | Permanently delete the account: keys revoked, files deleted, billing stopped. **No refund** of any balance. Type the account ID to confirm; `--yes` for scripts |
| `update [--check] [--version vX.Y.Z]` | Install the latest release after verifying it against `SHA256SUMS` |
| `whoami` | Show current account identity: account ID, App ID and the *public* key (the private `app_key` is never printed) |
| `account [--json]` | Billing account dashboard (status, balance, usage, active credential). When packing would free stored blocks, a hint points at `tessera optimize` |
| `dashboard` | Print a one-time browser login link (10 minutes) |
| `dashboard --logout-all` | Sign out every browser session for this account |
| `topup <amount> [--asset <code>] [--invoice] [--json] [--wait\|--no-wait]` | Add funds through NOWPayments, $5 to $10,000: a deposit address in a chosen coin (USDC on Base by default; `--asset xmr`, `btc`, `eth`, `usdttrc20`…), or a browser invoice in any accepted coin (`--invoice`). `tessera topup --help` explains both |
| `topup --minimums [--asset <code>] [--json]` | Show the $5 to $10,000 range and a coin's processor minimum for a deposit address |
| `topup --coins [--json]` | List the coins accepted for top-ups |
| `status` | Account funded (yes/no) + file count + total storage. JSON key `ready` is unchanged. Transfers still run their own host-funding check |
| `list [prefix]` | Files with real paths, sizes and dates (dates in your local time zone, labelled) |
| `upload <path> [--as <remote>] [--quiet]` | Upload a file **or a whole folder**. During technical validation a file over 40 MiB is refused before upload; with a balance, any size |
| `download <path> [--out <file>] [--offset <bytes>] [--length <bytes>]` | Download + SHA-256 verification; with `--offset`/`--length` only that byte range (only the slabs covering it are fetched). Interrupted downloads restart from the beginning; resume by hand with `--offset` = bytes already on disk |
| `delete <path> [--purge]` | Move to trash (or purge permanently) |
| `mv <src> <dst> [--force] [--json]` | Rename or move a stored file or folder without transferring data. A destination ending in `/` (or an existing remote folder) keeps the file name. A source that names a folder moves every file under it. The old path is marked deleted so `tessera sync` on other machines renames the local copy instead of uploading it again; a local edit those machines had not synced yet is uploaded at the new path. Re-running an interrupted folder move sends the remaining files to the same destination folder. `--force` replaces an existing destination (moved to the trash). |
| `optimize [prefix] [--dry-run] [--yes] [--json]` | Repack files in under-filled 40 MiB blocks into as few blocks as possible. Shows the plan (files, stored size now and after, one-off download+upload cost from live prices, monthly savings, payback, estimated time) and asks before running. `--dry-run` prints the plan only; `--yes` skips the prompt. |
| `share <path>` | Create a 30-day share link |
| `fetch <url> [name]` | Download a shared file |
| `sync add\|list\|status\|conflicts\|remove` | Manage folder sync |
| `sync watch` | Continuous two-way sync |
| `folder add [--install-service]\|open\|status\|watch\|remove` | The drop-in folder |
| `service install [--yes]\|uninstall\|status\|print\|start\|stop\|restart` | Background watcher |
| `find <pattern> [--content]` | Search paths, or file contents |
| `versions <path> [--restore <n>]` | List or restore retained versions (1 kept by default) |
| `trash list\|restore\|empty\|rm` | Recover soft-deleted files |
| `audit [path] [--verify] [--limit <n>]` | Check the redundancy each object was stored with; `--verify` also downloads each object (without saving it) and compares its SHA-256 with the hash stored at upload |
| `usage [--by-folder] [--cost] [--json]` | Storage usage; with billing credentials a billing section (logical, stored and network size, downloads, balance, days left) |
| `index --rebuild` | Rebuild the local metadata index |
| `config` | Show or change settings |
| `service install\|status\|print` | Background watcher definitions |
| `help` | Show help |

Add `--json` to `list`, `status`, `find`, `sync`, `usage`, `config`, `index`,
`audit`, `topup`, `account`, `mv`, and `optimize` for scripting. `topup --json` works on the
default address, invoice, `--coins`, and `--minimums` paths. `signup --json` prints the account id and its one-time API
key and recovery phrase (or, with `--secrets-file <path>`, only the file path).

## Billing and agent accounts

The CLI can create and manage an account on its own. `signup` opens an account
and logs this machine in in one step, without a browser; `login --code` connects
this machine with a one-time code shown on `/account`. Credentials are stored locally and never printed
again.

```bash
# Create an agent account. The tsk_… API key is printed exactly once.
tessera signup --name build-box --contact-email ops@example.com
tessera signup --json               # {"account_id": "…", "api_key": "…", …} for scripts
tessera signup --secrets-file ./tessera-secrets.txt   # secrets to a 0600 file, never stdout
tessera signup --force              # replace an existing API key with a new account

# Connect this machine with a code from /account (stores a long-lived API key)
tessera login --code ABCD-1234

# Recovery phrase on any machine (same account and App ID)
tessera login "word1 word2 ... word12"

# Dashboard and top-ups
tessera account                     # billing id, status, balance, usage, credential
tessera account --json
tessera topup 25                    # deposit address (USDC on Base by default)
tessera topup 25 --asset xmr        # deposit address in another coin (Monero, BTC, ETH, USDT…)
tessera topup 25 --invoice          # browser invoice: pick any accepted coin on the page
tessera topup 25 --json             # machine-readable create result
tessera topup 25 --wait             # poll until completed, expired or failed
tessera topup 25 --no-wait          # create and return immediately
tessera topup --coins               # list accepted coins
tessera usage --cost                # billing section + netted egress estimate
```

**Two ways to pay, both through NOWPayments.**

1. **Deposit address in a coin you choose** — `tessera topup <usd>` (USDC on
   Base) or `--asset <code>` (`xmr`, `btc`, `eth`, `sol`, `ltc`, `usdttrc20`,
   `usdtbsc`, … — 300+ coins; a bare ticker such as `usdt` lists its networks).
   Pay the quoted amount to the printed address from any wallet or exchange
   that supports that asset. A few coins have a processor minimum above $5
   for a deposit address.
2. **Browser invoice** — `tessera topup <usd> --invoice` opens a NOWPayments
   page where you pick any accepted coin (USDC, USDT, BTC, ETH, XMR, SOL, LTC
   and hundreds more).

Assets are shown in plain words ("USDC on Base", "Monero (XMR)"), and every
time is printed in your local time zone with the offset named, e.g.
`2026-10-31 02:13 UTC+07:00 (in 20 min)`. *Send before* is the deposit
address's rate quote (about 20 minutes, set by NOWPayments); *Invoice open
until* is the invoice link (24 hours). After the address window, run the
command again or use the invoice link.

On a terminal at least 60 columns wide, a deposit address is also printed as
a QR code. USDC on Base uses the same EIP-681 payment URI as before. Bitcoin,
Litecoin, Dogecoin, Bitcoin Cash, Monero, native Ether on Ethereum, and
USDC/USDT on Circle- or Tether-published EVM contracts use a payment URI that
includes the exact amount. Every other coin (TRC20, Solana, TON, XRP, and the
rest) encodes only the address — enter the amount shown above. If the
processor returns a memo or destination tag, the command prints it with the
address and warns that the QR does not include it. `--no-qr` hides the QR;
`--qr=address` encodes only the address, for exchange apps. There is no QR
for `--invoice`, `--json`, `--quiet`, or piped output.

**Top-ups default to a direct address.** A bare `topup <amount>` creates a
payment intent and prints the payment intent id, amount, exact amount/asset/
network to send, payment address, address expiry and invoice URL. In an
interactive terminal it polls until the intent is completed, expired or failed;
`--wait` and `--no-wait` override that. `--json` emits one document and does not
wait unless `--wait` is also given. A server that only returns an invoice URL
falls back to the previous invoice output, and `--invoice` asks for that path
explicitly. Before creating a deposit address the CLI reads that coin's processor minimum and, if the
amount is below it, refuses with the real number and creates nothing.

**Top-up range.** Top-ups are between $5 and $10,000 on every method (deposit
address or browser invoice). A few coins have a higher processor
minimum for a deposit address; the CLI and server refuse those amounts before
creating anything (`409 below_asset_minimum` with `minimum_usd`).
`tessera topup --minimums [--asset <code>]` prints the range and that coin's
minimum.

**Coins.** `topup --coins` lists the accepted coins; `--asset <code>` selects one
for the default address path. `--asset`, `--coin` and `--pay-currency` are
aliases, but do not name different assets in one command.

**One account, two credentials.** A stored `billing_session` (a human login)
wins over a `billing_agent_key`; `tessera account` prints which credential is
active. If the server rejects the session with a 401, the CLI clears it locally
and says *session expired — run `tessera login --email …`*; the next command
falls back to the stored API key, if there is one.

**Signup logs you in.** `signup` creates the billing account and this
machine's storage identity in the same step: it generates a recovery phrase,
derives the app key from it, registers a recovery key with Tessera and prints
the phrase and the API key once. Keep both: Tessera's servers never see the
phrase, and signup does not save it on this machine either (only the derived
app key and the API key are in `~/.tessera/config.json`). `tessera phrase show`
works only if you saved an encrypted copy of the phrase at `tessera login` —
the *phrase password* you chose there is local only, never sent to Tessera, and
is not an account login.

**Signup secrets in logs.** When signup's stdout is a terminal it prints the
phrase and API key once, with the warning. When stdout is a pipe, file or agent
log, it does **not** print them: it writes them to
`~/.tessera/signup-secrets-<account>.txt` (mode `0600`) and prints that path
plus an `rm` command for after you have stored them. `--secrets-file <path>`
picks the file (also with `--json`, which then carries `secrets_file` instead
of the secrets); `--show-secrets` prints them anyway. If the file cannot be
written the secrets are printed, so the phrase is never lost. Signup also
fetches the host list once and waits until enough hosts are funded for the
technical-validation upload.

**Seed login restores the same account.** On a new machine (or after
`rm -rf ~/.tessera`), `tessera login "<12 words>"` proves the phrase to Tessera,
gets back the same account ID and a fresh API key, and derives the same App ID —
headless, no browser, no password. A completed magic-link login with no storage
identity yet finishes the same registration.

**New accounts: technical validation.** Technical validation: one upload of
up to 40 MiB (41,943,040 bytes, one slab) and one download. Balance stays
**$0.00**. Tessera covers the hosts. The technical validation window starts
when your first device or agent connects, not when the account is created. Topping up
before using the technical validation ends it and the account works normally
from then on — there is no separate validation step after a top-up. After
the upload and download, top up to continue.
`tessera upload` of a larger file — a single file, or any file in a folder —
is refused before anything is sent to the hosts, so that one upload is not
used up. The refusal is skipped when the account status cannot be read.
Once the account has a balance, files of any size can be uploaded. A second
upload, or a second download, answers *Insufficient balance — top up to
continue* with the payment options.
`signup`, `status`, `account` and `usage` all show the same lines: what
technical validation still allows and whether uploads are allowed now.
`--json` has the same facts in a `trial` object (`state`, `balance_usd`,
`can_upload`, `top_up_required`; the JSON key names are unchanged). An account
in technical validation shows the network flag `host funding ready: false`
until its first top-up; that is expected. The CLI top-up path is NOWPayments
(a deposit address or a browser invoice); see *Top-up range* above for the $5
to $10,000 range and any higher per-coin processor minimum.

**Prices.** `tessera rates` prints the live price card from the billing system
(`GET /api/v1/billing/rates`, no login): storage **$2.50 per TB-month**
($0.000105 per 40 MiB block per 30 days), uploads **$1 per TB** ($0.000042 per
block), downloads **$10 per TB** ($0.000419 per block); one block uploaded,
stored 30 days and downloaded once costs $0.000566. TB = 10^12 bytes. Amounts
below $1 are shown with up to six decimals, from $1 up with two.

Before every `upload`, `download`, `fetch` and sync transfer the CLI checks
whether it can run. It dials each storage host over direct TCP (siamux, on the
port each host announces: usually 9984, some hosts use others; IP addresses first) and asks for this app
key's balance there. An upload needs 30 funded, reachable hosts; a download
needs 10 of the hosts that hold the file. The file name is checked first, so a
missing file fails at once with *not found*. While funding is still landing,
the CLI waits and prints `Waiting for host funding: N/30 hosts funded...` (up to
2 minutes, or 45 seconds without progress). If it cannot go ahead, it names the
cause:

- *only X of the 30 needed hosts are reachable from your network*: a firewall,
  VPN, proxy or sandbox is blocking direct outbound TCP to the hosts. The
  indexer lists every host by IP as well as by name, so DNS-only sandboxes
  (fake-IP DNS) work; a network that allows only HTTP(S) does not.
- *insufficient balance*: run `tessera topup 5`.
**Readiness.** `signup`, `login`, `status` and `account` print the readiness
block: *technical validation* (one upload and one download at $0.00), *top up
to continue* (technical validation used, or a $0 balance), or *ready*. Transfer commands (`upload`,
`download`, `sync`, `share`, `fetch`) mention it on stderr only when something
needs your action, such as a top-up — never during a working technical validation — and repeat
an unchanged line at most every 6 hours (`TESSERA_NO_READINESS_NOTICE=1` turns
it off).

**Progress.** `upload` and `download` draw a live progress bar on stderr
(phase — waiting for host funding, encoding, uploading shards X/30, pinning,
downloading — bytes done/total, percent, speed and ETA). When stderr is not a
terminal it prints a plain line every 10 s; `--json` and `--quiet` (or
`TESSERA_QUIET=1`) turn it off. stdout stays clean for scripts.

**Network hiccups and errors.** Read-only requests (GET/HEAD) retry up to 3
times on a dropped connection (a bare `EOF`, a reset, a TLS handshake timeout)
before failing, so a flaky sandbox link does not break `status`, `account` or
`usage`. A connection failure suggests `tessera login` only when the server
rejected the credentials; otherwise it says to check the network and retry.
Error messages never print URL query strings (indexer URLs carry request
signatures): they show `?<redacted>`. `tessera <command> --help` (upload,
download, delete, share, fetch, and the rest) prints that command's flags and
exits 0.

**Updates.** When a newer release exists, every command (including failed
commands and `--help`) ends with a one-line notice on stderr:
`New version vX.Y.Z available (you have vA.B.C). Run 'tessera update' to install it.`
`--json` output stays one JSON document on stdout; a JSON object also gets an
extra top-level `"update_available": {"current": "vA.B.C", "latest": "vX.Y.Z"}`
(only when an update is available; existing keys never change). The CLI
learns the latest version from the Tessera API (a response header on normal
requests, or `GET /api/v1/cli/latest`), falling back to GitHub, and refreshes
it in the background at most every 6 hours, so commands never wait for it.
Nothing is ever installed or forced: older versions keep working, and
`tessera config --update-check off` turns the notice off. `tessera update`
downloads the release asset, verifies it against `SHA256SUMS` and replaces the
binary. Requests carry `User-Agent: tessera-cli/<version> (<os>/<arch>)`.

**Deleting your account.** `tessera account delete` revokes every key,
deletes all stored objects, stops billing and marks the account deleted. Any
remaining balance is forfeited — there are no refunds. You must type the
account ID to confirm (`--yes` skips that for scripts). The website's Account
page has the same option.

**The signup key is shown once.** `signup` deliberately prints the new API key
one time, with a store-it warning, because the server never shows it again. No
other command echoes it. A second `signup` refuses to overwrite the stored key
unless you pass `--force`.

**`usage`: three sizes.** With a credential, `usage` appends a Billing block
with three sizes for the same files:

- *logical size*: your files' own bytes (a 1.7 MB PDF is 1.7 MB);
- *stored*: whole 40 MiB slabs, which is what storage is charged on. A single
  small file upload still takes a whole slab; folder uploads and `sync` pack
  small files together into shared slabs;
- *network size*: stored × 3. Each slab is erasure-coded into 30 pieces of
  4 MiB on 30 hosts, and any 10 rebuild it. So one 1.7 MB file shows about
  40 MiB stored and 120 MiB on the network.

It also shows downloads, balance and *days left*. During technical validation
*days left* reads "not applicable" (`days_left` is `null` in JSON). After you
top up, *days left* is balance ÷ today's daily storage charge for what you
store now. The technical-validation download shows the downloaded size as
"(1 technical-validation download, not billed)" in `egress_used_bytes`; after top-up, download cost is charged from measured host
spend (checked within about 15 minutes). When the account reports
`last_metered_at`, `usage` prints that time (and `usage --json` includes the
field); a server that omits it is unchanged. An estimate longer than 10 years
reads "more than 10 years at today's storage charge; transfers not included".
`--cost` prints a byte-based estimate only when the server reports egress in
bytes. Sizes use binary units
(KiB, MiB, GiB).
`usage --json` gains additive `billing` keys (or `billing_error`) only when
billing config exists; the old keys are unchanged. The `trial` object is the
preferred view of technical-validation state; overlapping fields are omitted from `readiness`
in `account --json` to avoid duplication. `trial_credit_usd` is deprecated
(always `"0"` / omitted on Money-10).

## Safety

- **Nothing is silently destroyed.** Deletes go to a trash namespace for 30 days
  by default (`tessera trash restore <path>`), and `--dry-run` previews any sync.
- **Downloads are atomic.** Data lands in a temp file and is renamed into place,
  so an interrupted sync never leaves a truncated file where a good one was.
- **Downloads are verified.** Every full download, including sync, is checked
  against the SHA-256 recorded at upload before it replaces an existing file.
  On a mismatch the command fails and the existing file is kept. Byte-range
  reads (`--offset`/`--length`) have no reference hash and are not checked.
- **Conflicts never lose data** under `keep-both`; under the default `newest`
  policy the losing copy is discarded, so choose `keep-both` if that matters.

## What happens under the hood

- **Auth**: ED25519 keypairs with time-bound URL signing — no long-lived tokens
- **Encryption**: ChaCha20-Poly1305 per object, keys never leave your device
- **Erasure coding**: Reed-Solomon 10+20 — data survives up to 20 host failures
- **Durability**: On-chain `Filesize` proof — the contract revision IS the durability assertion
- **Metadata**: Object names, **paths** and content hashes are encrypted alongside
  the data, not stored server-side
- **Sharing**: Cryptographic shared-object URLs with embedded encryption keys
  (no re-upload needed)
- **Sync**: Content-hash based three-way reconciliation over the indexer's
  append-only object-event log, which is what makes deletions propagate

## Config

Credentials and per-folder sync state live in
`~/.tessera/`:

```
~/.tessera/
  config.json        credentials and preferences
  index.json         local cache of stored objects
  trash.json         soft-deleted items
  sync/<root-id>/    per-folder state, cursor and lock
```

Set `TESSERA_HOME` to use a different directory (useful for several accounts).

```json
{
  "indexer_url": "https://index.tessera.storage",
  "billing_api_url": "https://tessera.storage/api/v1",  // billing API base
  "billing_session": "...",        // tessera_session cookie (human login) — local secret
  "billing_agent_key": "tsk_...",  // agent API key — local secret
  "app_id": "...",
  "app_key": "...",
  "phrase_encrypted": "...",       // optional — AES-GCM with Argon2id key
  "phrase_salt": "...",
  "version_retention": 0,          // 0 = keep 1 previous copy (default), negative = off
  "trash_retention_days": 0,       // 0 = 30 day default, negative = off
  "default_jobs": 0                // parallel transfers, 0 = automatic
}
```

`billing_session` and `billing_agent_key` are local secrets: the file is written
`0600`, the values are never logged or echoed by any command (except the
one-time `signup` print), and the file must never be committed or shared.

```bash
tessera config --version-retention 5   # keep 5 previous versions per file
tessera config --trash-retention 14
tessera config --default-jobs 4
tessera config --billing-url https://tessera.storage/api/v1
```

Saving an encrypted copy of the recovery phrase is optional — you'll be asked
on login. If you say yes, the phrase is encrypted with a *phrase password* of
your choice (Argon2id + AES-GCM) and saved locally so `tessera phrase show` can
print it later. The phrase password is never sent to Tessera and is not an
account login. Without it, you must keep the 12 words yourself.

`app_key` in this file is the **private** key derived from your phrase. It is
never printed; `tessera whoami` shows only its public half as *Public key*.

## Further reading

- `ARCHITECTURE.md` — how sync works, the metadata format, why deletions are
  tracked the way they are, and the roadmap.
- `TESTING.md` — unit tests, the two-machine end-to-end harness, and the manual
  checklist.

## Building from source

```bash
go build -mod=vendor -o tessera .   # vendor/ is committed; no checkouts needed
```

Requires Go 1.26+.

Run the tests (they cover the reconciliation table, ignore rules, path
normalisation and metadata encoding, and need no network access):

```bash
go test -mod=vendor ./...
```

Re-vendoring (a maintainer action, after either Sia fork changes) needs both
fork checkouts; see `TESTING.md` §2 for the exact procedure.

## Cross-compiling for release

```bash
# Linux (x86_64)
GOOS=linux GOARCH=amd64 go build -mod=vendor -o tessera-linux-amd64 .

# macOS Intel
GOOS=darwin GOARCH=amd64 go build -mod=vendor -o tessera-darwin-amd64 .

# macOS Apple Silicon
GOOS=darwin GOARCH=arm64 go build -mod=vendor -o tessera-darwin-arm64 .

# Windows (x86_64)
GOOS=windows GOARCH=amd64 go build -mod=vendor -o tessera-windows-amd64.exe .
```

The resulting binaries are **statically linked** — no dependencies needed. Ship
them as-is.