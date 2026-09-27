# Testing the Tessera CLI

Two layers of tests: a pure-logic unit suite that needs no network, and a
two-machine end-to-end scenario that exercises a real account.

## 1. Unit tests (no network, no credentials)

```bash
go test ./...
go test -race ./...     # optional
```

These cover the parts where a mistake costs data:

| Test | What it protects |
|---|---|
| `TestPlanReconcileTable` | The full three-way reconciliation table: upload, download, local delete, remote delete, conflicts under both policies |
| `TestRemoteDeleteIsNotResurrectedByLocalCopy` | Regression: a deleted file must not be pushed back up by a machine that still has a copy |
| `TestLocalEditBeatsRemoteDeleteWhenClearlyNewer` | The one exception — an edit made after the delete wins |
| `TestUnseenTombstoneIsAChange` | Deletion detection must not depend on current remote liveness |
| `TestLocalPathForRemote` | Remote paths map to the right root-relative path, and version/trash namespaces stay internal |
| `TestPlanSkipsVersionNamespace` | Retained versions are never synced as ordinary files |
| `TestBootstrapAdoptsMatchingContent` | A first sync must not re-download bytes already on disk |
| `TestBootstrapMergePicksTheNewerSide` | The default first-run merge policy, per file, plus `--adopt` overrides |
| `TestUploadBookkeepingSharedWithSync` | `tessera upload` and `tessera sync` share one source of truth |
| `TestUploadBookkeepingIgnoresOtherRoots` | Bookkeeping is scoped to the owning sync folder |
| `TestNormalizeRelPath` | Windows and POSIX separators, and path-escape rejection |
| `TestMetadataRoundTrip` / `TestMetadataLegacy` | The metadata envelope fits the indexer's 1024-byte limit, and pre-sync objects still resolve |
| `TestIgnoreRules` / `TestIgnoreFile` | Ignore semantics, including directory rules and negation |
| `TestStateRoundTrip` | Sync state survives a save/load cycle, including the cursor |
| `TestParseByteSize`, `TestFlagParsing`, `TestSameMTime` | CLI parsing and second-precision timestamp comparison |

## 2. Test account

Tests that touch the network need an account. A dedicated test key is provided:

```
tesseraclitest
```

Use it at login:

```bash
tessera login tesseraclitest
```

Because the app key is derived from `(phrase, app id, shared secret)`, this key
produces the same account on every machine — which is exactly what makes the
two-machine sync test meaningful. The first login still requires approving the
connection request in a browser, because approval is what establishes the shared
secret; it cannot be bypassed by design.

For an isolated sandbox, point `TESSERA_HOME` at a scratch directory:

```bash
export TESSERA_HOME=/tmp/tessera-test
```

## 3. Two-machine end-to-end test

The scenario simulates a Mac and a Windows PC sharing one account. Both
machines use the same credentials in separate `TESSERA_HOME` directories, so a
single machine can play both roles.

```bash
# Build first
go build -o tessera .

# Copy an authenticated config into two fake machines
STAMP=$(date +%s)
ROOT=/tmp/tessera-e2e
mkdir -p "$ROOT/homeA" "$ROOT/homeB" "$ROOT/mac/Documents" "$ROOT/win/tessera/macos/Documents"
cp ~/.tessera/config.json "$ROOT/homeA/config.json"
cp ~/.tessera/config.json "$ROOT/homeB/config.json"

A="env TESSERA_HOME=$ROOT/homeA ./tessera"
B="env TESSERA_HOME=$ROOT/homeB ./tessera"
REMOTE=tessera/e2e-$STAMP

# Machine A creates the folder and uploads it
mkdir -p "$ROOT/mac/Documents/notes"
echo "quarterly report" > "$ROOT/mac/Documents/report.txt"
echo "notes"           > "$ROOT/mac/Documents/notes/2026-09.txt"
echo "junk"            > "$ROOT/mac/Documents/.DS_Store"
$A upload "$ROOT/mac/Documents" --as "$REMOTE"

$A sync add "$ROOT/mac/Documents" --as "$REMOTE"
RA=$($A sync list --json | python3 -c "import json,sys;print(json.load(sys.stdin)[0]['id'])")
$A sync --root "$RA"

# Machine B adopts the same remote prefix
$B sync add "$ROOT/win/tessera/macos/Documents" --as "$REMOTE"
RB=$($B sync list --json | python3 -c "import json,sys;print(json.load(sys.stdin)[0]['id'])")
$B sync --root "$RB"

# 1. identical trees, and ignored files absent
diff -r "$ROOT/mac/Documents" "$ROOT/win/tessera/macos/Documents"
test ! -e "$ROOT/win/tessera/macos/Documents/.DS_Store" && echo "ignored OK"

# 2. new file on B arrives on A
echo "from windows" > "$ROOT/win/tessera/macos/Documents/from-win.txt"
$B sync --root "$RB"; $A sync --root "$RA"
cat "$ROOT/mac/Documents/from-win.txt"

# 3. delete on B removes it on A
rm "$ROOT/win/tessera/macos/Documents/notes/2026-09.txt"
$B sync --root "$RB"; $A sync --root "$RA"
test ! -e "$ROOT/mac/Documents/notes/2026-09.txt" && echo "delete B->A OK"

# 4. sync again: the file must stay deleted (no resurrection)
$A sync --root "$RA"; $B sync --root "$RB"
test ! -e "$ROOT/mac/Documents/notes/2026-09.txt" && echo "no resurrection OK"

# 5. rename propagates without re-uploading
mv "$ROOT/mac/Documents/report.txt" "$ROOT/mac/Documents/renamed.txt"
$A sync --root "$RA"; $B sync --root "$RB"
test -e "$ROOT/win/tessera/macos/Documents/renamed.txt"

# 6. conflict: default newest-wins
echo "A" > "$ROOT/mac/Documents/c.txt"; $A sync --root "$RA"; $B sync --root "$RB"
sleep 1.2
echo "B newer" > "$ROOT/win/tessera/macos/Documents/c.txt"; $B sync --root "$RB"
sleep 1.2
echo "A newest" > "$ROOT/mac/Documents/c.txt"; $A sync --root "$RA"
$B sync --root "$RB"; $A sync --root "$RA"
grep -q "A newest" "$ROOT/win/tessera/macos/Documents/c.txt" && echo "newest-wins OK"

# 7. conflict: keep-both
$A sync add "$ROOT/mac/keep" --as "$REMOTE-keep" --policy keep-both
# ... repeat an edit on both sides; both copies must survive and
#     `$A sync conflicts` must list the conflict copy.

# 8. dry-run must not change anything
echo "x" > "$ROOT/mac/Documents/dry.txt"
$A sync --root "$RA" --dry-run
test ! -e "$ROOT/win/tessera/macos/Documents/dry.txt" && echo "dry-run OK"

# 9. trash round-trip
$A delete "from-win.txt"     # soft delete
$A trash list
$A trash restore "from-win.txt"
```

Clean up when finished, because every test uploads real data:

```bash
$A list "$REMOTE" --json | python3 -c "
import json,sys
for f in json.load(sys.stdin)['files']:
    print(f['path'])
" | while read -r p; do $A delete "$p" --purge; done
```

## 4. Manual checklist

Some behaviour is easier to confirm by hand than to script:

- [ ] `tessera folder add` creates a folder, syncs it and opens it in Finder/Explorer
- [ ] Dragging a file into the folder and running `tessera sync` uploads it
- [ ] `tessera sync watch` reports changes and stops cleanly on Ctrl-C
- [ ] Editing a large file mid-sync does not corrupt it (downloads are atomic)
- [ ] `tessera list` shows real paths, not object ids
- [ ] `tessera versions <path> --restore 1` restores an older copy
- [ ] `TESSERA_HOME=/tmp/other tessera whoami` shows separate settings
- [ ] Config migration: a config pointing at `https://index.dithr.dev` is
      rewritten to `https://index.tessera.storage` on first use

## Bugs the end-to-end tests caught

These were all found by the two-machine scenario or the live account, and each
now has a regression test:

1. **Deletes did not propagate** when the decision was keyed off current remote
   liveness instead of the observed tombstone — a file deleted on one machine
   was pushed back up by the other.
2. **A deleted file could be resurrected** on a second sync pass because
   "remote is no longer live" was read as "upload the local copy".
3. **`tessera upload` and `tessera sync` disagreed**: upload wrote only to the
   local index, so the next sync treated the file as new on both sides and
   downloaded its own upload.
4. **A first sync re-downloaded identical content** instead of adopting what was
   already on disk — expensive on a large pre-existing folder.
5. **`upload <dir> --as X` collapsed every file onto one remote path** instead
   of using X as a prefix.
6. **Folder uploads ignored `.tesseraignore`**, so `.DS_Store` and `.git` were
   uploaded.
7. **Remote paths were used as root-relative paths**, so synced files landed in
   a wrong subfolder.
8. **A retired indexer host produced an opaque TLS error** instead of a
   migration; configs pointing at `index.dithr.dev` are now rewritten.
9. **Ignore rules were not applied to remote objects**, so a peer's junk could
   be materialised locally.
10. **Deduplication stole the original path.** An object has exactly one
    logical path, so re-pointing an object that lived at another path made the
    first path vanish. Content reuse is now path-scoped, and a re-attach is
    verified against the indexer before it is reported as successful.
11. **A remote file that was merely absent locally was unpinned.** The
    "missing locally" branch assumed a local deletion, which destroyed conflict
    copies that existed on other machines.
12. **A deleted path could be resurrected after a move**, because the deletion
    was forgotten immediately. Deletions now leave a bounded tombstone.
13. **`delete`/`versions` acted on a stale local index**, producing "object not
    found" after work on another machine. Mutating commands now rebuild it.
14. **A local deletion was invisible to the planner when the remote object was
    unchanged.** Neither "content changed" test fired, so the file was never
    unpinned and peers kept it forever — this is why renames appeared to
    duplicate rather than move.
15. **A pushed deletion left no tombstone**, so the object's own deletion event
    could not be resolved to a path and the next sync downloaded the file back.
16. **The conflict record was cleared by the upload that followed it**, so
    `sync conflicts` reported "No conflicts recorded" even though conflict
    copies existed on both machines.

## Known test-environment limits

- Sia host throughput in a sandbox is slow (tens of seconds per small object).
  The end-to-end scripts are therefore run as background jobs.
- The indexer rejects `test`-flagged uploads; use the normal account.
- `go build` needs network access the first time to fetch the pinned
  dependencies (`go.sia.tech/siastorage`, `go.sia.tech/indexd`).