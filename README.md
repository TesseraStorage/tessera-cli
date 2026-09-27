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
tessera folder add                  # ~/Tessera by default
tessera folder add ~/Dropbox-ish --as tessera/drop
tessera folder open
tessera folder status
tessera folder watch                # keep it in sync in this terminal
```

Keep the watcher running in a terminal, or install it as a background service:

```bash
tessera service install   # writes launchd/systemd definitions for review
tessera service print     # just show the definitions
```

Files are downloaded for real (like Dropbox's "available offline" mode) — this
build does not mount a virtual filesystem, which keeps the single static binary
and avoids kernel drivers.

## Commands

| Command | Description |
|---|---|
| `login [phrase]` | Connect this machine (browser approval). An optional recovery phrase makes the account reproducible. |
| `logout` | Remove local credentials from `~/.tessera/` |
| `whoami` | Show current account identity |
| `status` | Account ready state + file count + total storage |
| `list [prefix]` | Files with real paths, sizes and dates |
| `upload <path> [--as <remote>]` | Upload a file **or a whole folder** |
| `download <path> [--out <file>]` | Download + SHA-256 verification |
| `delete <path> [--purge]` | Move to trash (or purge permanently) |
| `share <path>` | Create a 30-day share link |
| `fetch <url> [name]` | Download a shared file |
| `sync add\|list\|status\|conflicts\|remove` | Manage folder sync |
| `sync watch` | Continuous two-way sync |
| `folder add\|open\|status\|watch\|remove` | The drop-in folder |
| `find <pattern> [--content]` | Search paths, or file contents |
| `versions <path> [--restore <n>]` | List or restore retained versions (1 kept by default) |
| `trash list\|restore\|empty\|rm` | Recover soft-deleted files |
| `audit [path] [--verify]` | Check redundancy and integrity |
| `usage [--by-folder]` | Storage usage and a redundancy-adjusted total |
| `index --rebuild` | Rebuild the local metadata index |
| `config` | Show or change settings |
| `service install\|status\|print` | Background watcher definitions |
| `help` | Show help |

Add `--json` to `list`, `status`, `find`, `sync`, `usage`, `config`, `index`
and `audit` for scripting.

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
  "app_id": "...",
  "app_key": "...",
  "phrase_encrypted": "...",       // optional — AES-GCM with Argon2id key
  "phrase_salt": "...",
  "version_retention": 0,          // 0 = keep 1 previous copy (default), negative = off
  "trash_retention_days": 0,       // 0 = 30 day default, negative = off
  "default_jobs": 0                // parallel transfers, 0 = automatic
}
```

```bash
tessera config --version-retention 5   # keep 5 previous versions per file
tessera config --trash-retention 14
tessera config --default-jobs 4
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
go mod tidy
go build -o tessera .
```

Requires Go 1.26+.

Run the tests (they cover the reconciliation table, ignore rules, path
normalisation and metadata encoding, and need no network access):

```bash
go test ./...
```

## Cross-compiling for release

```bash
# Linux (x86_64)
GOOS=linux GOARCH=amd64 go build -o tessera-linux-amd64 .

# macOS Intel
GOOS=darwin GOARCH=amd64 go build -o tessera-darwin-amd64 .

# macOS Apple Silicon
GOOS=darwin GOARCH=arm64 go build -o tessera-darwin-arm64 .

# Windows (x86_64)
GOOS=windows GOARCH=amd64 go build -o tessera-windows-amd64.exe .
```

The resulting binaries are **statically linked** — no dependencies needed. Ship
them as-is.