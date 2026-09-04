# Tessera CLI

Command-line client for [Tessera](https://siagate.dev) — decentralized storage on the Sia network.

**Encrypted. Erasure-coded. 30 hosts. No single point of failure.**

## Quickstart

### 1. Download

| Platform | Binary |
|---|---|
| **Linux** (x86_64) | `tessera-linux-amd64` |
| **macOS** (Apple Silicon) | `tessera-darwin-arm64` |
| **macOS** (Intel) | `tessera-darwin-amd64` |
| **Windows** (x86_64) | `tessera-windows-amd64.exe` |

### 2. Login

```bash
./tessera login
```

A browser tab opens. Enter the connect key you were given, click Approve. Save your 12-word recovery phrase — it's the **only** way back to your account.

### 3. Use

```bash
./tessera upload photo.png      # Erasure-coded across 30 Sia hosts
./tessera list                  # See all your files
./tessera download photo.png    # Get it back, SHA-256 verified
./tessera share photo.png       # Create a 30-day share link
./tessera delete photo.png      # Remove from your account
./tessera status                # File count + total storage used
./tessera logout                # Remove local credentials
```

## Commands

| Command | Description |
|---|---|
| `login` | Connect this machine to Tessera (browser-based approval) |
| `logout` | Remove local credentials from `~/.tessera/` |
| `whoami` | Show current account identity |
| `status` | Account ready state + file count + total storage |
| `list` | All files with name, size, and date |
| `upload <path>` | Upload a file (10+20 erasure coding, pinned to Sia) |
| `download <name>` | Download a file + SHA-256 hash verification |
| `delete <name>` | Unpin a file from Sia |
| `share <name>` | Create a shareable link (valid 30 days) |
| `fetch <url> [name]` | Download a shared file from a link |
| `help` | Show this help |

## What happens under the hood

- **Auth**: ED25519 keypairs with time-bound URL signing — no long-lived tokens
- **Encryption**: ChaCha20-Poly1305 per object, keys never leave your device
- **Erasure coding**: Reed-Solomon 10+20 — data survives up to 20 host failures
- **Durability**: On-chain `Filesize` proof — the contract revision IS the durability assertion
- **Metadata**: Object names and types are encrypted alongside the data, not stored server-side
- **Sharing**: Cryptographic shared-object URLs with embedded encryption keys (no re-upload needed)

## Config

Credentials stored at `~/.tessera/config.json`:

```json
{
  "indexer_url": "https://index.dithr.dev",
  "app_id": "...",
  "app_key": "...",
  "phrase_encrypted": "...",   // optional — AES-GCM with Argon2id key
  "phrase_salt": "..."
}
```

Recovery phrase encryption is optional — you'll be asked on login. If enabled, the phrase is encrypted with a master password of your choice and saved locally. Without it, you must keep the 12 words yourself.

## Building from source

```bash
go mod tidy
go build -o tessera .
```

Requires Go 1.26+.

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

The resulting binaries are **statically linked** — no dependencies needed. Ship them as-is.