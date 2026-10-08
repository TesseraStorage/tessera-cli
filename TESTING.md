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
| `TestServiceDefinitionsAreBranded` | The service definitions carry no unrelated vendor name |
| `TestPlistContent` / `TestUnitContent` | The launchd agent and systemd unit contain the required keys |
| `TestPlanServicePathsAreAbsolute` | A definition is never written to a relative path |
| `TestInstallServiceWritesDefinitionAndActivates` | Installing writes the definition and runs the platform command |
| `TestInstallServiceWithoutActivationChangesNothing` | Without `--yes` no service is started |
| `TestPromptYesNoNonInteractive` | A piped stdin is never prompted, so scripts cannot hang |
| `TestCmdSignupColdStart` / `TestSignupGuardAgainstOverwrite` | Cold-start signup persists a 0600 config, prints the key once, and only `--force` may replace it |
| `TestCredentialLabel` / `TestSession401ClearsAndFallsBackToAgentKey` | Credential precedence: a stored session wins, and a rejected session falls back to the agent key |
| `TestHTTPBillingErrorShapes` / `TestParseBillingError` | Both moneyd error shapes parse and preserve `Extra` fields |
| `TestUsageStorageOutputUnchanged` / `TestUsageCostHuman` | The storage report is unchanged without billing config; `--cost` nets the allowance |

### Billing commands (fake `billingAPI`, no network)

The billing commands (`signup`, `login --email`, `account`, `topup`, `promo`,
and the `usage` billing section) are tested without any network. Two patterns
cover them:

- **Command tests** replace the `newBillingClient` package var — the same
  package-level seam style as `serviceCommand` and `statusCommand` — with an
  in-memory fake that embeds `billingAPI` and implements only the method under
  test. The embedded nil interface panics on any other call, so the fake also
  pins which endpoint a command is allowed to speak to (see `fakeSignupAPI` in
  `signup_test.go`, `fakeAccountAPI` in `account_test.go`). `t.Setenv` plus
  `t.TempDir()` isolate the 0600 config, and assertions check its mode and JSON
  keys, not just the printed text.
- **Client-shape tests** run the real `httpBilling` client against an
  `httptest` loopback server and assert the exact method, path, JSON body, and
  the `Authorization` header or `tessera_session` cookie. Fakes must answer
  errors in moneyd's real shapes: nested `{"detail":{"error":"…"}}` for most
  routes, flat `{"error":"…"}` for `/auth/verify`'s 401 and the x402
  `502 settlement_pending`.

No test may dial `tessera.storage` or any non-loopback host — CI has no network.
`TESSERA_BILLING_URL` exists for development and loopback tests only; it is
never the production default.

## 2. Vendored Sia forks (no checkouts, no network)

The CLI depends on two local Sia forks, wired in `go.mod` with absolute
`replace` directives:

```go
replace go.sia.tech/siastorage => /root/siastorage-fork
replace go.sia.tech/indexd => /root/tessera
```

Both are required together. A dependency's own `replace` directives are
ignored, and the `siastorage` fork calls `appClient.ObjectHosts`, which exists
only in the local `indexd` fork; with only one replace the build does not
compile. The generated `vendor/` tree is committed, so CI and anyone without
the checkouts builds and tests with:

```bash
go vet -mod=vendor ./...
go test -mod=vendor ./...
go build -mod=vendor .
```

`-mod=vendor` reads only `vendor/`; the absolute paths above never need to
exist on a build runner (they do not exist on GitHub Actions).

CI runs the same gates: the test job does `go vet -mod=vendor ./...` and
`go test -mod=vendor ./...`, and the build matrix cross-compiles all four
targets with `-mod=vendor` (linux-amd64, darwin-amd64, darwin-arm64,
windows-amd64 — see `.github/workflows/build.yml`). The full local gate before a
release adds `gofmt` and disables the test cache:

```bash
gofmt -l *.go                    # must print nothing
go vet -mod=vendor ./...
go test -mod=vendor -count=1 ./...
GOOS=linux   GOARCH=amd64 go build -mod=vendor -o /tmp/tessera-linux-amd64 .
GOOS=darwin  GOARCH=amd64 go build -mod=vendor -o /tmp/tessera-darwin-amd64 .
GOOS=darwin  GOARCH=arm64 go build -mod=vendor -o /tmp/tessera-darwin-arm64 .
GOOS=windows GOARCH=amd64 go build -mod=vendor -o /tmp/tessera-windows-amd64.exe .
```

### Re-vendoring after a fork change

Whenever either fork changes, refresh the committed tree from a clean fork
checkout:

```bash
# The fork keeps an ignored 62 MB GeoLite2-City.mmdb in its package
# directory; move it out first so `go mod vendor` does not copy it.
mv /root/siastorage-fork/GeoLite2-City.mmdb /tmp/
cd /root/tessera-cli
go mod vendor
mv /tmp/GeoLite2-City.mmdb /root/siastorage-fork/
git add go.mod go.sum vendor
```

`vendor/` should be ≈ 27 MB and contain no `*.mmdb` file. `go mod tidy` (and
anything else that resolves the module graph) requires **both** fork checkouts
to exist at the replace paths above; a missing checkout fails with an error
naming the path.

### If the replaces are dropped

Dropping either `replace` while `vendor/modules.txt` still records it makes
every `-mod=vendor` command fail with `inconsistent vendoring`. There are only
two consistent end states: keep both replaces and the vendored tree in
lockstep, or publish both forks and pin pseudo-versions (out of scope for
now).

## 3. Test account

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

## 4. Two-machine end-to-end test

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

## 5. Manual checklist

Some behaviour is easier to confirm by hand than to script:

- [ ] `tessera folder add` creates a folder, syncs it and opens it in Finder/Explorer
- [ ] `tessera folder add` asks "Keep this folder in sync automatically? [Y/n]"
      and installs the watcher when answered yes (verified with a pty)
- [ ] `tessera folder add | cat` does **not** prompt, and prints the `--yes` hint
- [ ] `tessera service install` changes nothing; `--yes` installs and starts
- [ ] `tessera service status` reports `Running: yes` only while it really runs
- [ ] `tessera service uninstall --yes` removes it and the folder keeps its files
- [ ] Tests leave `~/.tessera` untouched (no `sync/testroot` appears)
- [ ] Dragging a file into the folder and running `tessera sync` uploads it
- [ ] `tessera sync watch` reports changes and stops cleanly on Ctrl-C
- [ ] Editing a large file mid-sync does not corrupt it (downloads are atomic)
- [ ] `tessera list` shows real paths, not object ids
- [ ] `tessera versions <path> --restore 1` restores an older copy
- [ ] `TESSERA_HOME=/tmp/other tessera whoami` shows separate settings
- [ ] A config pointing at an unsupported indexer host fails with an actionable
      message instead of an opaque TLS error

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
   migration; an unsupported host is now rejected with a clear message.
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
- `go build` and `go test` need no network: the two Sia forks and every other
  dependency are vendored (see §2). Only re-vendoring (`go mod vendor`) and
  `go mod tidy` need the fork checkouts, and `go mod tidy` may need the module
  proxy for unrelated dependencies.