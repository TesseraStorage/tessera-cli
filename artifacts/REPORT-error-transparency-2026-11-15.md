# REPORT — Sonnet packet: full error transparency (SDK + CLI)

**Date:** 2026-11-15
**Status:** implemented, tested, committed on feature branches in both repos. No release, no deploy (per packet).

## What changed

**`/root/siastorage-fork`** (branch `feat/error-transparency-2026-11-15`, commit `5f024d9`, cut from `master` at `e80a3fc`):
- New `attemptring.go`: `attemptRing` keeps the last 5 failed per-host attempts (host key prefix — `ed25519:` + first 12 hex chars — underlying error text truncated to ~120 chars, elapsed as `%.1fs`). `wrap(err)` attaches the formatted ring to `err` via `%w`, so `errors.Is/As` still sees through to `ErrNoMoreHosts`/`ErrNotEnoughShards`/a context error.
- `upload.go` (`uploadShard`): records every failed write result into the ring; every shard-ending error (`ErrNoMoreHosts` once the pool is exhausted, and the two `ctx.Err()` exits) is now `ring.wrap(...)`'d. The one `ErrNoMoreHosts` that fires before any host was ever tried (`pickInitial` failing immediately) is left unwrapped on purpose — the ring is empty, nothing is lost.
- `sdk.go` (download's host-attempt loop): same pattern — every failed `ReadSector` result is recorded, and `ErrNotEnoughShards` plus the loop's `ctx.Err()` exit are wrapped.
- `PinObject` and `fetchHosts` were checked and left unchanged — they already wrap with `%w` and have no per-host attempt loop to summarize, so there's nothing to add there.
- New tests: `TestUpload/exhausted_hosts_carry_attempt_detail` (forces all 30 hosts to fail writes with a distinct marker error, asserts the final error contains a host prefix, the marker text, and an elapsed duration) and an extension of the existing `TestDownload/timeout` asserting the same three things for the download path. Full `go test ./...` green (121s before my edits were added, ~188s after — no behavior change, same host counts).

**`/root/tessera-cli`** (branch `feat/error-transparency-2026-11-15`, commit `dd110a5`, cut from `main` at `51e56a0`):
- Vendored the same diff into `vendor/go.sia.tech/siastorage/{sdk.go,upload.go}` plus the new `attemptring.go`, by hand-applying the identical patch rather than running `go mod vendor` — see "Known issue" below.
- Audited the CLI's own fatal paths (`files.go`, `readiness.go`) per the packet's part 2 ("confirm every fatal path uses `%w`, don't add a shorter summary"): `cmdUploadPath`/`cmdDownload` already print `fatal("%v", explainTransferError(cfg, err, true))`. `explainTransferError` only rewrites a **payment-required (402)** error into top-up guidance; any other error, including a host-shortage error now carrying the attempt ring, passes through **unchanged** (confirmed by reading the function — the comment already says so). No code change was needed here; the CLI already satisfies the packet's rule.
- One pre-existing, deliberate exception found and left alone (out of scope — it's payment/door behavior): `shortageAfterPaymentRequired` substitutes the earlier-seen 402 detail for a host-shortage error **only** when a 402 was already seen in this same process. That's intentional product behavior from a prior packet, not something hiding SDK detail in the normal case — the packet's own "do not change door behavior" line covers it.
- `go build ./...` clean. `go test ./...` green, except one pre-existing, order-dependent flaky test (`TestFreshCacheMakesNoNetworkCall`) — confirmed to fail/pass identically with and without this change across repeated full-suite and isolated runs; not a regression.

## Known issue — could not run `go mod vendor`

Both repos' `go.mod`/`go.sum` are already out of sync with what's in the local module cache: even a **completely clean checkout with zero edits** fails `go build ./...`/`go mod vendor` with "updates to go.mod needed," and running `go mod tidy` to fix it pulls in a large (~40-80 line), unrelated dependency-version bump across both repos. This is pre-existing and unrelated to this packet. I avoided committing that noise by building/testing with `GOFLAGS=-mod=mod` (which lets the build proceed without persisting the go.mod/go.sum change) in `siastorage-fork`, and by hand-applying the exact same diff to tessera-cli's vendor copy instead of running `go mod vendor` there (vendor consistency checking is package-list-based, not content-hash-based, so this is safe and `go build ./...` confirms it). **Flagging this stale go.mod/go.sum state as a separate, standing problem** in both repos that will need a dedicated `go mod tidy` pass and review at some point, independent of this packet.

## Scope note — other siastorage-fork replacers not touched

`siastorage-fork` is also the `replace` target for `hostevents`, `fleetsdk`, `skeensdk`, and `canarysdk`/`arniewriter` inside `/root/tessera`. This packet scoped the work to `tessera-cli` only ("Repo: tessera-cli (SDK lives in vendor / siastorage)"), so I did not rebuild/retest those other modules against the fork change. The change is purely additive (same function signatures, same error sentinels, `errors.Is/As` still works) so it should be low-risk for them, but they have not been verified — flagging per the cross-repo rule rather than silently deciding it doesn't matter.

## Do-not list checked
No retry/timeout/attempt-limit/host-selection/pool changes. No indexd/hostfilter/funding changes. No new flag — this is the default error text now. No release, no deploy — both repos sit on feature branches only.

## Commits
- `siastorage-fork` `feat/error-transparency-2026-11-15` @ `5f024d9`
- `tessera-cli` `feat/error-transparency-2026-11-15` @ `dd110a5`

## Open items
1. The stale go.mod/go.sum in both repos (see "Known issue") — worth a dedicated cleanup pass.
2. Whether to also rebuild/retest `hostevents`/`fleetsdk`/`skeensdk`/`canarysdk` against the fork change (not done, out of this packet's stated scope).
3. No decision needed on a CLI release — packet says "no release."
