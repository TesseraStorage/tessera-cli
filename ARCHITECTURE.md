# Architecture and roadmap

This document explains how folder sync works, why it is built the way it is, and
what is planned next. It is the design companion to `README.md` (usage) and
`TESTING.md` (verification).

## 1. The problem with the original client

The pre-sync CLI was a thin wrapper over `siastorage`: one file in, one file out.
Every object carried a single `{"name": "<file>"}` metadata field, and lookups
matched on the base filename only. That has three consequences:

1. **No paths.** `Documents/report.txt` and `Pictures/report.txt` were the same
   logical file. Folders were impossible.
2. **No identity.** `Object.ID()` is derived from the upload's random data key,
   so re-uploading modified content produces a *different* object. Anything that
   tracks a file over time needs its own identity, and the only user-controlled
   channel is metadata.
3. **No local state.** Nothing on disk recorded what had been synced, so there
   was no basis for deciding what changed.

Sync needed all three fixed.

## 2. What a file looks like now

Every object carries a small metadata envelope (the indexer caps metadata at
1024 bytes, so this is deliberately tight — roughly 250 bytes):

```json
{
  "name": "report.pdf",
  "v": 1,
  "tessera": {
    "root": "e9e47e1f72937cdb",
    "path": "notes/2026/report.pdf",
    "sha256": "d14bd71e…",
    "size": 18234,
    "mode": "0644",
    "mtime": "2026-09-25T12:00:00.123Z",
    "machine": "macbook-pro"
  }
}
```

Three decisions matter here:

- **`path` is always slash-separated**, even on Windows. A folder synced from
  Windows is addressable from macOS and Linux.
- **`root` scopes the path.** Two independent sync roots can hold
  `notes/report.pdf` without colliding.
- **`sha256` is the source of truth for "did it change"**, not timestamps.
  Timestamps in the metadata exist only to decide conflicts.

Objects without a `tessera` block (uploaded by an older client) still resolve by
`name`, so the new client can read everything the old one wrote.

## 3. Reconciliation

Sync is a three-way comparison per path:

| input | source |
|---|---|
| **base** | what this machine last agreed on (`state.json`) |
| **local** | the file on disk now (streamed SHA-256) |
| **remote** | the newest object event for that path (or a tombstone) |

| base | local | remote | action |
|---|---|---|---|
| = local | = | = | nothing |
| = | changed | = | upload |
| = | = | changed | download |
| = | changed | changed | conflict |
| = | deleted | = | delete remotely |
| = | = | tombstoned | delete locally |
| = | changed | tombstoned | upload only if *strictly* newer |
| = | deleted | changed | download (remote wins) |
| never seen | matches remote | live | adopt, no transfer |
| never seen | differs | live | newer side wins (`--adopt` overrides) |

Two rules are load-bearing:

- **Deletions propagate as tombstones.** The indexer's `object_events` table is
  append-only and preserves `was_deleted` rows, and deletes are never pruned, so
  a machine that has been offline for weeks still learns about a deletion.
- **A delete beats an old local copy.** When a peer deleted a file and this
  machine merely has a stale identical copy, the file is deleted here too.
  Only an edit *strictly newer* than the deletion resurrects it. Without this
  rule, renames and deletions fight each other and files come back.

## 4. Why deletion needed its own bookkeeping

Three separate omissions all produced the same visible symptom — "I renamed a
file and now I have two, or the old one came back" — and each needed its own fix:

1. **A local deletion was invisible.** With the remote object untouched, neither
   "content changed" test fired, so the planner skipped the path and the object
   was never unpinned. Fixed by diffing the scan against the ledger
   (`markMissingLocal`).
2. **A pushed deletion left no record.** The object's own deletion event could
   then no longer be resolved to a path, so the remote view kept the file alive
   and the next sync re-downloaded it. Fixed by leaving a bounded tombstone
   (`TombstonedAt`, pruned after 30 days).
3. **A file absent locally was treated as deleted.** That branch unpinned remote
   objects that existed elsewhere — which destroyed conflict copies on the peer
   that created them. Now the distinction is explicit: a path is deleted only if
   the ledger had agreed on it *and* the file is gone.

## 5. Local state

Per sync root, under `~/.tessera/sync/<root-id>/`:

| file | purpose |
|---|---|
| `config.json` | local path, remote prefix, conflict policy, last sync |
| `state.json` | per-path base hash, object id, cached remote view, cursor, tombstones, conflict ledger |
| `lock` | advisory lock so two syncs cannot race on one folder |

`state.json` holds three maps:

- `entries` — this machine's agreements, plus deletion tombstones.
- `remote` — a materialised view of the peer/network state, updated
  incrementally through `cursor` rather than re-read each run.
- `conflicts` — original path → conflict copy. A dedicated ledger, because it
  must persist even when a sync decides to do nothing.

Writes are atomic (temp file + rename), so a crash leaves the previous
consistent view.

**Format choice:** JSON, not SQLite. SQLite in Go needs cgo (breaking the
static single-binary promise) or a new pure-Go dependency. A compiled in-memory
index over compact JSON is enough for personal-scale folders, and the storage
interface is narrow enough to swap later.

## 6. Conflict handling

Default policy is **newest wins**, because for a personal folder the least
surprising outcome is "the copy I edited last". For users who cannot afford to
lose a version, `--policy keep-both` preserves the loser as:

```
report (conflicted copy, WINDOWS-PC 2026-09-25-143005).pdf
```

Details that make this work:

- The conflict copy is **materialised on the machine that created it**, not just
  uploaded. Otherwise the next scan sees a tracked path with no file, calls it a
  deletion, and unpins the copy it just made.
- Every peer recognises the marker phrase and records the same conflict, so
  `tessera sync conflicts` agrees everywhere.
- Deleting the copy resolves the record.
- Repeats get a numeric suffix rather than clobbering an earlier copy.

## 7. What sync does not do

- **No server-side folders.** A remote prefix is a string in metadata; there is
  no directory object. Empty directories are therefore not synced.
- **No virtual filesystem.** Files are really downloaded (Dropbox's "available
  offline" model). See the roadmap for the Files-On-Demand path.
- **No crdt/vector clocks.** Conflict detection is hash plus mtime. That is
  sufficient for "same file edited on two machines" and avoids a coordination
  service; it is not sufficient for concurrent edits *within* one file.
- **Unchanged files are not re-hashed.** The hash cache keys on size + mtime at
  second precision. An edit that preserves both is invisible; this is the same
  tradeoff rsync and every file-sync tool makes.

## 8. Related commands

| command | mechanism |
|---|---|
| `trash` | re-attaches the object under `.tessera-trash/<sha8>/<path>`; purged after the retention window |
| `versions` | re-attaches the superseded object under `.tessera-versions/<path>/<stamp>-<sha8>` |
| `audit` | reads slab redundancy from object metadata; `--verify` downloads and hashes |
| `usage` | sums logical bytes and reports the ≈3× network footprint from 10+20 coding |
| `find` | searches decrypted paths (and optionally content) from the local index |
| `index` | rebuilds the local object cache used by read-only commands |

Mutating commands rebuild the index first. Acting on a stale cache produced
"object not found" after work on another machine, which is exactly the kind of
failure that erodes trust in a storage tool.

## 9. The background watcher

`tessera sync` is a one-shot command, so nothing keeps a folder in step unless
a watcher is running. The watcher is a per-user service:

| platform | mechanism | definition |
|---|---|---|
| macOS | launchd user agent | `~/Library/LaunchAgents/io.tessera.watcher.plist` |
| Linux | systemd user unit | `~/.config/systemd/user/tessera-watch.service` |
| Windows | Scheduled Task at logon | Task `Tessera` |

Design decisions worth recording:

- **Nothing is installed without an explicit yes.** `service install` writes the
  definition and then either prompts (`Keep this folder in sync automatically?
  [Y/n]`) or, when stdin is not a terminal, prints the exact `--yes` command.
  A CLI that silently registers a login item is a CLI people stop trusting.
- **The activation command is real, not a suggestion.** For launchd the agent
  must first be `bootout` (best effort) and then `bootstrap`ed; `kickstart`
  alone fails on an agent launchd has never loaded. systemd needs no bootstrap
  step. Windows uses `schtasks /create /sc onlogon`.
- **`service status` reports running state, not file existence.** It queries
  `launchctl print` or `systemctl --user is-active`, because a definition on
  disk says nothing about whether sync is happening.
- **Definitions are pure functions** (`plistContent`, `unitContent`,
  `planService`) and the executor is injectable, so the exact installed content
  and the exact commands are asserted in tests without mutating the host.

## 10. Roadmap

Ordered by value ÷ effort, with the reasoning for each.

**Next**

- **Files On-Demand.** The one thing that still separates this from iCloud
  Drive: a placeholder that materialises on open. macOS FSKit / Windows Cloud
  Files API are the right primitives (OS-native, no kernel extension); FUSE is
  the fallback. Design cost is high and it must stay opt-in, because it gives up
  the static-binary promise on those platforms.
- **Native change notification.** Replacing the polling watcher with
  FSEvents / inotify / ReadDirectoryChangesW makes `sync watch` sub-second.
  The re-scan path already handles correctness, so this is purely latency.
- **Folder sharing.** Share a sync root with another *account* read-only or
  read-write, with a per-share key. This turns backup into collaboration and is
  the single biggest gap versus Dropbox.
- **Selective sync.** Per-root include/exclude beyond `.tesseraignore`, so a
  large root does not flood a laptop.

**Later**

- **Durability notifications.** `audit` on a schedule, with a warning when a
  file's redundancy drops — the promise is 20 host failures, so the tool should
  say when that margin narrows.
- **Cost controls.** Budget alerts and a per-folder cost estimate, since Sia
  pricing lives in contracts rather than a simple per-GB rate.
- **Multi-account profiles.** `TESSERA_HOME` already allows it manually; a
  `--profile` flag makes it a feature.
- **Publish.** Serve an immutable, verifiable static site from a prefix.
- **Mirroring.** Optional S3/B2/restic mirror for users who want a second,
  conventional copy while they build trust in the network.
- **Shell completions** for all commands, and `--json` everywhere (already
  present on the read-only commands).

**Explicitly not planned**

- Server-side dedup or manifest changes (network/SDK side, not the client).
- A hosted web UI (different product).
- Client-side content deduplication across paths: an object has exactly one
  logical path, so pointing a second path at it removes the first. Reusing
  storage would require an alias table and a fundamentally different object
  model; the current client uploads a fresh object instead, and says so.