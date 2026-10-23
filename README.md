# Tessera CLI

Command-line client for [Tessera](https://tessera.storage) — decentralized storage on the Sia network.

**Encrypted. Erasure-coded. 30 hosts. No single point of failure.**

Sync folders across your machines, drag files into a Tessera folder, search,
version and verify everything you store — all from one static binary.

## Quickstart

### macOS / Linux

```bash
curl -fsSL https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.sh | bash
```

### Windows (PowerShell)

```powershell
powershell -c "irm https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.ps1 | iex"
```

### Manual download

| Platform | Binary |
|---|---|
| **Linux** (x86_64) | `tessera-linux-amd64` |
| **macOS** (Apple Silicon) | `tessera-darwin-arm64` |
| **macOS** (Intel) | `tessera-darwin-amd64` |
| **Windows** (x86_64) | `tessera-windows-amd64.exe` |

### Next steps

```bash
tessera login                    # Browser opens — enter your connect key, approve, save 12 words
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
# On the Mac: log in and SAVE THE 12 WORDS
tessera login

# On Windows: log in with that SAME recovery phrase
tessera login "word1 word2 ... word12"
```

The app key is derived from the phrase, so the same phrase means the same
account. A plain `tessera login` on the second machine generates a *new* phrase
and a *separate account* — the two machines would never see each other's files,
no matter how the folders are configured. (You can also set `TESSERA_PHRASE`.)

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
| `login [phrase]` | Connect this machine (browser approval). An optional recovery phrase makes the account reproducible. |
| `login --email <e> [--token <t>]` | Billing sign-in with a magic link: request it, then complete with the emailed token |
| `login --billing-logout` | End the billing session (an agent key, if any, is untouched) |
| `signup [--name <n>] [--contact-email <e>] [--json] [--force]` | Create an agent billing account; the API key is shown once |
| `logout` | Remove local credentials from `~/.tessera/` |
| `whoami` | Show current account identity |
| `account [--json]` | Billing account dashboard (status, balance, allowance, active credential) |
| `topup <amount> [--pay-currency <asset>] [--x402 [--payment <b64>]]` | Add funds: a NOWPayments invoice, or an externally signed x402 USDC payment |
| `promo <code>` | Redeem a promo code on the billing account |
| `status` | Account ready state + file count + total storage |
| `list [prefix]` | Files with real paths, sizes and dates |
| `upload <path> [--as <remote>]` | Upload a file **or a whole folder** |
| `download <path> [--out <file>]` | Download + SHA-256 verification |
| `delete <path> [--purge]` | Move to trash (or purge permanently) |
| `share <path>` | Create a 30-day share link |
| `fetch <url> [name]` | Download a shared file |
| `sync add\|list\|status\|conflicts\|remove` | Manage folder sync |
| `sync watch` | Continuous two-way sync |
| `folder add [--install-service]\|open\|status\|watch\|remove` | The drop-in folder |
| `service install [--yes]\|uninstall\|status\|print\|start\|stop\|restart` | Background watcher |
| `find <pattern> [--content]` | Search paths, or file contents |
| `versions <path> [--restore <n>]` | List or restore retained versions (1 kept by default) |
| `trash list\|restore\|empty\|rm` | Recover soft-deleted files |
| `audit [path] [--verify]` | Check redundancy and integrity |
| `usage [--by-folder] [--cost] [--json]` | Storage usage; with billing credentials a billing section, and `--cost` adds the netted egress estimate |
| `index --rebuild` | Rebuild the local metadata index |
| `config` | Show or change settings |
| `service install\|status\|print` | Background watcher definitions |
| `help` | Show help |

Add `--json` to `list`, `status`, `find`, `sync`, `usage`, `config`, `index`,
`audit`, and `account` for scripting. `signup --json` prints the account id and
its one-time API key.

## Billing and agent accounts

The CLI can create and manage a billing account on its own. `signup` opens an
agent account without a browser; `login --email` signs a human in with a magic
link. Both credentials are stored locally and never printed again.

```bash
# Create an agent account. The tsk_… API key is printed exactly once.
tessera signup --name build-box --contact-email ops@example.com
tessera signup --json               # {"account_id": "…", "api_key": "…"} for scripts
tessera signup --force              # replace an existing agent key with a new account

# Human sign-in with a magic link (stores a session cookie, not a key)
tessera login --email you@example.com
tessera login --email you@example.com --token <token>   # completes the sign-in
tessera login --billing-logout      # ends the session; an agent key is untouched

# Dashboard, top-ups and promos
tessera account                     # billing id, status, balance, allowance, credential
tessera account --json
tessera topup 25                    # NOWPayments hosted invoice
tessera topup 25 --pay-currency btc
tessera topup 25 --x402             # print the challenge to sign externally
tessera topup 25 --payment <base64 X-PAYMENT>   # submit the signed authorization
tessera promo WELCOME10
tessera usage --cost                # billing section + netted egress estimate
```

**One account, two credentials.** A stored `billing_session` (a human login)
wins over a `billing_agent_key`; `tessera account` prints which credential is
active. If the server rejects the session with a 401, the CLI clears it locally
and says *session expired — run `tessera login --email …`*; the next command
falls back to the stored agent key, if there is one.

**Login finishes setup.** A completed magic-link login (or a plain
`tessera login` when only an agent key is stored) registers this machine's
storage identity headlessly when the account does not have one yet: the CLI
mints a one-time connect key, approves the connection itself, derives the app
key from a recovery phrase (generated and printed once unless `TESSERA_PHRASE`
or a phrase argument supplies one), and activates the link. If the account
already has an identity, it says so and points at reusing
`~/.tessera/config.json` from the machine that registered it (or contacting
support). `signup` alone does not mint storage credentials.

**The signup key is shown once.** `signup` deliberately prints the new API key
one time, with a store-it warning, because moneyd never shows it again. No other
command echoes it. A second `signup` refuses to overwrite the stored key unless
you pass `--force`.

**Signing stays external.** `topup --x402` prints the payment challenge
(network, asset, pay-to address, atomic amount, challenge nonce and timeout)
and stops. You sign the EIP-3009 authorization with your own wallet tooling and
hand the base64 payload back through `--payment`; the CLI never holds a wallet
or signing key. `topup` has no `--json` yet — its output is human-readable.

**`usage` gained a billing section.** Without billing credentials the storage
output is exactly what it always was. With a credential, `usage` appends a
Billing block (open slabs, egress used vs allowance, balance, days left) and
`--cost` adds a PAYG egress estimate that nets the allowance
(`max(0, used − allowance) × $5/TB`, decimal TB) and is labelled an estimate.
`usage --json` gains additive `billing` keys (or `billing_error`) only when
billing config exists; the old keys are unchanged.

## Safety

- **Nothing is silently destroyed.** Deletes go to a trash namespace for 30 days
  by default (`tessera trash restore <path>`), and `--dry-run` previews any sync.
- **Downloads are atomic.** Data lands in a temp file and is renamed into place,
  so an interrupted sync never leaves a truncated file where a good one was.
- **Downloads are verified.** Every fetch is checked against the recorded
  SHA-256 before it replaces an existing file.
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

Credentials and per-folder sync state live in `~/.tessera/`:

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
  "billing_api_url": "https://tessera.storage/api/v1",  // moneyd API base
  "billing_session": "...",        // tessera_session cookie (human login) — local secret
  "billing_agent_key": "tsk_...",  // agent API key — local secret
  "app_id": "...",
  "app_key": "...",                // 128 hex chars: full 64-byte Ed25519 private key, NOT a 32-byte seed
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

### Injecting a pre-issued credential

If you already have a credential (e.g. a storage-only app key handed to you by
an operator, or minted through a headless redemption flow) you do not need to
run `tessera login` at all — write `~/.tessera/config.json` directly:

```json
{
  "indexer_url": "https://index.tessera.storage",
  "app_id": "<64-hex-char app ID>",
  "app_key": "<128-hex-char full private key>"
}
```

`app_key` must be the **full 64-byte Ed25519 private key as 128 hex
characters** (`seed(32) || public_key(32)`) — exactly what the SDK's
`appKey().export()` produces. A bare 32-byte seed (64 hex characters) is not
enough: it decodes as valid hex and will be accepted by nothing that checks
hex-validity alone, but fails ed25519 signing. The CLI now rejects a
wrong-length key immediately with a clear error instead of failing deep
inside a transfer.

```bash
tessera config --version-retention 5   # keep 5 previous versions per file
tessera config --trash-retention 14
tessera config --default-jobs 4
tessera config --billing-url https://tessera.storage/api/v1
```

Recovery phrase encryption is optional — you'll be asked on login. If enabled,
the phrase is encrypted with a master password of your choice and saved locally.
Without it, you must keep the 12 words yourself.

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