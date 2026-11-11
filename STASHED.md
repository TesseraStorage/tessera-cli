# Stashed: local wallet commands and CLI x402

The CLI no longer ships `tessera wallet init|show|fund`, `tessera topup --wallet`,
or the `--x402` / `--payment` challenge flow. The only CLI deposit path is
NOWPayments (a deposit address by default, or `--invoice` for a hosted invoice).

To restore the removed code and docs:

- Branch: `stash/local-wallet-2026-11-11`
- Commit: `9b7c25229b5796943156c609d76ff81496058c2a` (`Add tessera mv and tessera optimize (#35)`, also tagged `v1.15.0`)

```
git fetch origin stash/local-wallet-2026-11-11
git checkout stash/local-wallet-2026-11-11
```

Or inspect the diff against this tree:

```
git diff 9b7c25229b5796943156c609d76ff81496058c2a HEAD -- wallet.go wallet_test.go topup.go main.go README.md ARCHITECTURE.md
```

The stashed tree includes `wallet.go`, `wallet_test.go`, and the previous
`topup` / README / ARCHITECTURE wording. x402 payments remain available through
the HTTP API; see the developer docs.
