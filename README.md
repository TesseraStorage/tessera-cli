# Tessera CLI

Command-line client for [Tessera](https://tessera.storage) — decentralized storage on the Sia network.

**Encrypted. Erasure-coded. 30 hosts. No single point of failure.**

Upload, download, search, version and verify everything you store — all from
one static binary.

## Quickstart

### macOS / Linux

```bash
curl -fsSL https://tessera.storage/install.sh | sh
# same script, from the release: curl -fsSL https://github.com/TesseraStorage/tessera-cli/releases/latest/download/install.sh | sh
# a specific version: ... | sh -s v1.20.2
```

Current release: **v1.20.2**. `tessera version` reports the git tag the release
workflow stamps into the binary. The same release also publishes `install.sh`
and `install.ps1` (`https://github.com/TesseraStorage/tessera-cli/releases/latest/download/install.sh`),
and lists both in its `SHA256SUMS`.

To check the installer itself before running it, download it and `SHA256SUMS`
from the same release and verify:

```bash
base=https://github.com/TesseraStorage/tessera-cli/releases/latest/download
curl -fsSLO "$base/install.sh" && curl -fsSLO "$base/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS   # must print "install.sh: OK" (macOS: shasum -a 256 -c)
sh install.sh
```

The installer verifies the binary against the release's `SHA256SUMS`, never
uses sudo and installs to `~/.local/bin`. If that directory is not on your
`PATH` it adds one marked line to your shell's startup file (`~/.zshrc`,
`~/.bashrc`/`~/.bash_profile`, fish's `config.fish`, else `~/.profile`): it
asks first when it can reach your terminal, and does it automatically, saying
which file it changed, when there is no terminal (agents, CI). Open a new shell
or run `export PATH="$HOME/.local/bin:$PATH"` (fish: `set -gx PATH "$HOME/.local/bin" $PATH`)
to use it right away. `tessera uninstall` removes the binary and that marked line.

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
```

## Ignoring files

A `.tesseraignore` at the root of an uploaded folder uses gitignore-style
patterns. `.DS_Store`, `Thumbs.db`, `desktop.ini`, editor temp files and
partial downloads are always ignored.

```
*.log
build/
!important.log
```

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
| `account delete [--yes]` | Permanently delete the account: keys revoked, files deleted, billing stopped. **No refund** of any balance. Type the account ID to confirm; `--yes` for scripts. Afterwards this machine's login, file index, trash list and update-check cache are removed from `~/.tessera` |
| `account purge-data [--yes] [--json]` | Permanently delete **all files** now (every object deleted, every slab unpinned; cannot be undone) and keep the account, its API key and its balance. Works at any balance, including $0. Asks `Type y to confirm`; without a terminal `--yes` is required. Also clears the local trash list and file index |
| `update [--check [--json]] [--version vX.Y.Z]` | Install the latest release after verifying it against `SHA256SUMS`. `--check` only reports; `--check --json` prints `current`, `latest` and `up_to_date` (plus `update_available` when newer) |
| `whoami [--json]` | Show current account identity: indexer, App ID and the *public* key (the private `app_key` is never printed). `--json`: `indexer`, `app_id`, `public_key`, `phrase_saved_locally`, `local_check_only` (always `true`). Reads the local config only: it exits 0 even when the key is missing or no longer valid; `status` checks the key with the indexer |
| `account [--json]` | Billing account dashboard (status, balance, usage, active credential). When packing would free stored blocks, a hint points at `tessera optimize` |
| `dashboard` | Print a one-time browser login link (10 minutes) |
| `dashboard --logout-all` | Sign out every browser session for this account |
| `topup <amount> [--asset <code>] [--invoice] [--json] [--wait\|--no-wait]` | Add funds through NOWPayments, $5 to $10,000: a deposit address in a chosen coin (USDC on Base by default; `--asset xmr`, `btc`, `eth`, `usdttrc20`…), or a browser invoice in any accepted coin (`--invoice`). `tessera topup --help` explains both |
| `topup --minimums [--asset <code>] [--json]` | Show the $5 to $10,000 range and a coin's processor minimum for a deposit address |
| `topup --coins [--json]` | List the coins accepted for top-ups |
| `status` | Account funded (yes/no, or unknown when the balance cannot be read, e.g. no billing key on this machine) + file count + total storage. JSON key `ready` is unchanged. Transfers still run their own host-funding check |
| `list [prefix]` | Files with real paths, sizes and dates (dates in your local time zone, labelled). Trashed files and earlier versions are not listed (see `trash list` and `versions`); `--json` gives `"files": []` when there are none |
| `upload <path> [--as <remote>] [--quiet] [--json]` | Upload a file **or a whole folder** (a folder without `--as` goes under `tessera/<folder name>/`). Unknown options, an invalid `--as` (absolute, `..`, or inside `.tessera-trash`/`.tessera-versions`) and unreadable files are refused before anything else, including the balance check. During technical validation a file over 40 MiB is refused before upload; with a balance, any size. Uploading to a path that already holds a file keeps the previous copy as an earlier version (see `versions`). `--json`: `ok`, `uploaded`, `failed`, `bytes`, `files` (path, size, sha256, object_id), `errors`. Retrying an upload that already went through stores a second copy (an earlier version with the same content; there is no idempotency key): before retrying, check `list --json` for the path with the same `sha256`. `tessera optimize` removes such identical earlier copies |
| `download <path> [--out <file>] [--offset <bytes>] [--length <bytes>]` | Download + SHA-256 verification; with `--offset`/`--length` only that byte range (only the slabs covering it are fetched). `--out` is checked before anything is downloaded (a folder, or a location that cannot be written, is refused; missing folders are created). Interrupted downloads restart from the beginning; resume by hand with `--offset` = bytes already on disk. Ctrl-C removes the partial `.tessera-tmp-*` file. One left by a killed download is removed by the next download into that folder once it is 15 minutes old; a download that is still running, even a stalled or stopped one, holds its file locked and it is never removed. If the partial file disappears anyway, the download fails with a clear "run the download again" error and the existing file is left unchanged |
| `delete <path>\|<folder>/ [--purge] [--yes]` | Move a file to the trash, or every file under a folder (`delete photos/`). `--purge` deletes permanently, with the file's earlier versions. Asks for confirmation in a terminal; without a terminal (scripts, agents) `--yes` is required, otherwise nothing changes and the exit status is 1. When a file has an earlier version, trashing it makes the newest earlier version the current file (the command says so) |
| `mv <src> <dst> [--force] [--json]` | Rename or move a stored file or folder without transferring data. A destination ending in `/` (or an existing remote folder) keeps the file name. A source that names a folder moves every file under it. The old path is marked deleted. Re-running an interrupted folder move sends the remaining files to the same destination folder. `--force` replaces an existing destination (moved to the trash). |
| `optimize [prefix] [--dry-run] [--yes] [--json]` | Repack files in under-filled 40 MiB blocks into as few blocks as possible. Before repacking it removes earlier versions that are byte-identical (same SHA-256) to the current file at that path; earlier versions with different content are kept. Shows the plan (files, stored size now and after, one-off download+upload cost from live prices, monthly savings, payback, estimated time) and asks before running. `--dry-run` prints the plan only; `--yes` skips the prompt. |
| `find <pattern> [--content]` | Search paths, or file contents |
| `versions <path> [--restore <n>] [--json]` | List the earlier versions of a file (uploading to a path that already holds a file keeps the previous copy), or make one current again with `--restore <n>` (the copy that was current is kept as an earlier version). Earlier versions stay stored and count toward storage until `delete <path> --purge` (or, when byte-identical to the current file, `optimize`). `--json` is an array, `[]` when there are none; its keys are PascalCase, unlike `upload --json`, and stay that way for compatibility: `ObjectID`, `Path`, `Name`, `Root`, `SHA256`, `Size`, `ModTime`, `Machine`, `Updated`, `Legacy` |
| `trash list\|restore\|empty\|rm` | Recover soft-deleted files. Trashed files never expire automatically; they stay, and count toward storage, until `trash rm` or `trash empty`. The trash list is per machine (`~/.tessera/trash.json`): it shows only files trashed from this machine. Restoring an item whose trashed copy is no longer stored (for example after `account purge-data`) says so and removes it from the list |
| `audit [path] [--verify] [--limit <n>]` | Check the redundancy each object was stored with; `--verify` also downloads each object (without saving it) and compares its SHA-256 with the hash stored at upload |
| `usage [--by-folder] [--cost] [--json]` | Storage usage; with billing credentials a billing section (logical, stored and network size, downloads, balance, days left). Trashed files are counted under `(trash)` |
| `index [--rebuild] [--json]` | Show or rebuild the local file index (a cache of your file list) |
| `config [--json]` | Show or change settings (`--trash-retention <days>`: negative turns the trash off; any other value keeps it on, with no automatic expiry; `--update-check on\|off`, `--billing-url <url>`). `--version-retention` and `--default-jobs` (1 to 64) are accepted but only affect folder sync, which is not available yet |
| `help` | Show help |

Add `--json` to `list`, `status`, `find`, `usage`, `config`, `index`,
`audit`, `topup`, `account`, `mv`, `optimize`, `upload`, `versions`, `whoami`
and `update --check` for scripting. JSON key names never change between
releases; empty lists are `[]`, not `null`. Errors are not always JSON: a
usage error or bad input found before the command runs, and a balance block
before an upload, print plain text to stderr and exit non-zero, so check the
exit status before parsing stdout. `topup --json` works on the
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
written the secrets are printed, so the phrase is never lost. A
`--secrets-file` path is recorded as an absolute path, so `tessera secrets done`
and the reminder find the same file from any folder. Signup also fetches the
host list once, so funding for the technical-validation hosts starts; it does
not probe hosts or wait.

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

A transfer uses the hosts the network returns. There is no preflight probe: the
CLI does not dial hosts or wait for funding before the transfer. The file name
is checked first, so a missing file fails at once with *not found*. If a
transfer cannot run, the error is the one the billing system or the network
returns:

- *insufficient balance*: the account must be topped up. Run `tessera topup 5`.
- a host or network error: the client's network must allow direct outbound TCP
  to the providers (siamux, on the port each one announces, usually 9984). Host
  lists carry IP addresses, so fake-IP DNS sandboxes work; a network that
  allows only HTTP(S) does not.

**Readiness.** `signup`, `login`, `status` and `account` print the readiness
block: *technical validation* (one upload and one download at $0.00), *top up
to continue* (technical validation used, or a $0 balance), or *ready*. Transfer commands (`upload`,
`download`) mention it on stderr only when something
needs your action, such as a top-up — never during a working technical validation — and repeat
an unchanged line at most every 6 hours (`TESSERA_NO_READINESS_NOTICE=1` turns
it off).

**Progress.** `upload` and `download` draw a live progress bar on stderr
(phase — uploading shards, pinning, downloading — bytes done/total, percent,
speed and ETA). When stderr is not a
terminal it prints a plain line every 10 s; `--json` and `--quiet` (or
`TESSERA_QUIET=1`) turn it off. stdout stays clean for scripts.

**Network hiccups and errors.** Read-only requests (GET/HEAD) retry up to 3
times on a dropped connection (a bare `EOF`, a reset, a TLS handshake timeout)
before failing, so a flaky sandbox link does not break `status`, `account` or
`usage`. A connection failure suggests `tessera login` only when the server
rejected the credentials; otherwise it says to check the network and retry.
Error messages never print URL query strings (indexer URLs carry request
signatures): they show `?<redacted>`. `tessera <command> --help` (upload,
download, delete, and the rest) prints that command's flags and
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
  small file upload still takes a whole slab; folder uploads pack
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

- **Nothing is silently destroyed.** Deletes go to a trash namespace and stay
  there until you run `tessera trash rm` or `tessera trash empty` (there is no
  automatic expiry; `tessera trash restore <path>` brings one back). The trash
  list is kept per machine in `~/.tessera/trash.json`. A delete without a terminal
  needs `--yes`. Re-uploading to an existing path keeps the previous copy
  (`tessera versions <path>`).
- **Downloads are atomic.** Data lands in a temp file (`.tessera-tmp-*`) and is
  renamed into place, so an interrupted download never leaves a truncated file
  where a good one was. Ctrl-C removes the temp file; one left by a killed
  download is removed by the next download into the same folder once it has
  not changed for 15 minutes. A running download holds a lock on its temp file
  (on Windows, the open file cannot be deleted), so a slow, stalled or stopped
  download never loses its file to another download.
- **Downloads are verified.** Every full download is checked
  against the SHA-256 recorded at upload before it replaces an existing file.
  On a mismatch the command fails and the existing file is kept. Byte-range
  reads (`--offset`/`--length`) have no reference hash and are not checked.

## What happens under the hood

- **Auth**: ED25519 keypairs with time-bound URL signing — no long-lived tokens
- **Encryption**: ChaCha20-Poly1305 per object, keys never leave your device
- **Erasure coding**: Reed-Solomon 10+20 — data survives up to 20 host failures
- **Durability**: On-chain `Filesize` proof — the contract revision IS the durability assertion
- **Metadata**: Object names, **paths** and content hashes are encrypted alongside
  the data, not stored server-side

## Config

Credentials and local state live in `~/.tessera/`:

```
~/.tessera/
  config.json        credentials and preferences
  index.json         local cache of stored objects
  trash.json         soft-deleted items
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
  "version_retention": 0,          // folder sync only (not available yet)
  "trash_retention_days": 0,       // negative = trash off; otherwise on, no automatic expiry
  "default_jobs": 0                // folder sync only (not available yet), 1 to 64
}
```

`billing_session` and `billing_agent_key` are local secrets: the file is written
`0600`, the values are never logged or echoed by any command (except the
one-time `signup` print), and the file must never be committed or shared.

```bash
tessera config --trash-retention -1   # turn the trash off (delete is immediate)
tessera config --update-check off
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

- `ARCHITECTURE.md` — the metadata format and internal design notes.
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