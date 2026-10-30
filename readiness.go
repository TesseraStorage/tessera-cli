package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	rhp4 "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	rhp "go.sia.tech/coreutils/rhp/v4"
	"go.sia.tech/coreutils/rhp/v4/siamux"
	"go.sia.tech/indexd/hosts"
)

// Transfer readiness.
//
// indexd's Ready flag is a repair-redundancy threshold, not "can this account
// move data". A free (born-paused) agent stays Ready:false until its first paid
// top-up, yet hostfilter funds its host accounts on its first GET /hosts. A
// paid account is funded by indexd itself. Neither flag tells the user whether
// an upload can actually succeed from where they are: that depends on how many
// hosts they can reach and how many of those already hold a balance for this
// app key.
//
// So before an upload or download the CLI measures readiness directly. It asks
// every usable host (siamux, the same transport the SDK dials) for its settings
// and for this app key's account balance. Both RPCs are free and unsigned. Then:
//
//   - reachable < needed: the user's network cannot reach enough hosts. Fail
//     with "only X of N needed hosts reachable from your network".
//   - funded >= needed: ready, so go.
//   - otherwise, if the billing balance is known and is <= 0: fail with
//     "insufficient balance".
//   - otherwise funding is still landing (usually a few seconds). Re-fetch the
//     host list, which re-triggers the free-path funding, and poll until enough
//     hosts are funded or the wait expires ("account not ready yet").
//
// Upload needs dataShards+parityShards hosts (30). Download needs dataShards (10).
// TESSERA_SKIP_PREFLIGHT=1 skips the probe (the SDK errors are still mapped).

const (
	readinessCodeNetwork      = "hosts_unreachable"
	readinessCodeBalance      = "insufficient_balance"
	readinessCodeNotReady     = "account_not_ready"
	readinessCodeHostListFail = "host_list_unavailable"
)

// readinessError is the user-facing replacement for the SDK's raw
// "no more hosts available" / "not enough shards" errors.
type readinessError struct {
	Code      string
	Message   string
	Reachable int
	Funded    int
	Needed    int
	Total     int
}

func (e *readinessError) Error() string { return e.Message }

// hostProbe is the per-host result of one readiness probe.
type hostProbe struct {
	Reachable bool
	Funded    bool
}

type readinessDeps struct {
	listHosts func(ctx context.Context) ([]hosts.HostInfo, error)
	probe     func(ctx context.Context, h hosts.HostInfo, upload bool) hostProbe
	balance   func() (float64, bool) // billing balance in USD, known
	sleep     func(time.Duration)
	now       func() time.Time
	progress  func(funded, needed int)
	maxRounds int // >0: classify after this many probe rounds instead of waiting
	// balanceGrace is how long a $0 billing balance is tolerated before it is
	// reported as insufficient. A new free agent's promo credit is posted by
	// hostfilter together with its first host funding, a few seconds after the
	// first host-list fetch, so $0 at the start is not yet conclusive.
	balanceGrace time.Duration
	// stallWait gives up early when enough hosts are reachable but the funded
	// count has not grown for this long (funding is not going to land soon).
	stallWait time.Duration
}

var (
	readinessPollInterval = 3 * time.Second
	readinessMaxWait      = 120 * time.Second
	readinessProbeTimeout = 8 * time.Second
	readinessBalanceGrace = 30 * time.Second
	readinessStallWait    = 45 * time.Second
)

// newReadinessDeps wires the probe to the real indexer, hosts and billing API.
var newReadinessDeps = func(cfg *Config) (readinessDeps, error) {
	client, appKey, err := connectAPI(cfg)
	if err != nil {
		return readinessDeps{}, err
	}
	acct := rhp4.Account(appKey.PublicKey())
	return readinessDeps{
		listHosts: func(ctx context.Context) ([]hosts.HostInfo, error) { return client.Hosts(ctx, appKey) },
		probe: func(ctx context.Context, h hosts.HostInfo, upload bool) hostProbe {
			return probeHost(ctx, h, acct, upload)
		},
		balance:      func() (float64, bool) { return billingBalanceUSD(cfg) },
		balanceGrace: readinessBalanceGrace,
		stallWait:    readinessStallWait,
		sleep:        time.Sleep,
		now:          time.Now,
		progress: func(funded, needed int) {
			if quietMode {
				return
			}
			if transferProgress != nil {
				transferProgress.Phase(fmt.Sprintf("waiting for host funding: %d/%d hosts funded", funded, needed))
				return
			}
			fmt.Fprintf(os.Stderr, "Waiting for host funding: %d/%d hosts funded...\n", funded, needed)
		},
	}, nil
}

// probeHost dials one host over siamux and checks this account's balance
// against the cost of one sector write (upload) or read (download).
//
// Addresses are tried IP-literal first, each with its own share of the
// timeout: on sandboxed networks with fake-IP DNS a hostname dial hangs until
// its deadline, and it must not eat the budget of the IP address the indexer
// also lists for the same host. siamux authenticates the host key, so the
// order cannot let a different machine answer for the host.
func probeHost(ctx context.Context, h hosts.HostInfo, acct rhp4.Account, upload bool) hostProbe {
	ctx, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
	defer cancel()
	addrs := orderedSiamuxAddrs(h)
	for i, addr := range addrs {
		remaining := len(addrs) - i
		dctx := ctx
		var dcancel context.CancelFunc = func() {}
		if dl, ok := ctx.Deadline(); ok && remaining > 1 {
			dctx, dcancel = context.WithTimeout(ctx, time.Until(dl)/time.Duration(remaining))
		}
		t, err := siamux.Dial(dctx, addr, h.PublicKey)
		dcancel()
		if err != nil {
			continue
		}
		defer t.Close()
		res := hostProbe{Reachable: true}
		bal, err := rhp.RPCAccountBalance(ctx, t, acct)
		if err != nil || bal.IsZero() {
			return res
		}
		need := types.ZeroCurrency
		if s, err := rhp.RPCSettings(ctx, t); err == nil {
			if upload {
				need = s.Prices.RPCWriteSectorCost(rhp4.SectorSize).RenterCost()
			} else {
				need = s.Prices.RPCReadSectorCost(rhp4.SectorSize).RenterCost()
			}
		}
		res.Funded = bal.Cmp(need) >= 0
		return res
	}
	return hostProbe{}
}

// orderedSiamuxAddrs returns the host's siamux addresses with IP literals
// first, keeping the indexer's order otherwise.
func orderedSiamuxAddrs(h hosts.HostInfo) []string {
	var ips, names []string
	for _, a := range h.Addresses {
		if a.Protocol != siamux.Protocol {
			continue
		}
		host, _, err := net.SplitHostPort(a.Address)
		if err == nil && net.ParseIP(host) != nil {
			ips = append(ips, a.Address)
		} else {
			names = append(names, a.Address)
		}
	}
	return append(ips, names...)
}

// billingBalanceUSD reads balance_usd from the billing account when a billing
// credential is stored. ok=false means unknown (no credential or API error),
// and unknown never blocks: the caller waits instead of claiming "no balance".
func billingBalanceUSD(cfg *Config) (float64, bool) {
	if credentialLabel(cfg) == "" {
		return 0, false
	}
	acct, err := newBillingClient(cfg).Account()
	if err != nil {
		return 0, false
	}
	switch v := acct["balance_usd"].(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	return 0, false
}

// checkReadiness implements the decision described at the top of this file.
func checkReadiness(ctx context.Context, d readinessDeps, needed int, upload bool) error {
	started := d.now()
	deadline := started.Add(readinessMaxWait)
	state := map[types.PublicKey]hostProbe{}
	firstRound, unreachableRetried := true, false
	lastFunded := -1
	lastProgressFunded, lastProgressAt := -1, started
	for round := 1; ; round++ {
		list, err := d.listHosts(ctx)
		if err != nil {
			return &readinessError{Code: readinessCodeHostListFail, Needed: needed,
				Message: fmt.Sprintf("could not fetch the storage host list from the indexer: %v", err)}
		}
		var candidates []hosts.HostInfo
		for _, h := range list {
			if upload && !h.GoodForUpload {
				continue
			}
			candidates = append(candidates, h)
		}

		// Probe hosts not yet known funded. Unreachable hosts are retried once
		// (on the second round) and then left alone.
		var todo []hosts.HostInfo
		for _, h := range candidates {
			prev, seen := state[h.PublicKey]
			if seen && prev.Funded {
				continue
			}
			if seen && !prev.Reachable && unreachableRetried {
				continue
			}
			todo = append(todo, h)
		}
		results := make([]hostProbe, len(todo))
		var wg sync.WaitGroup
		for i, h := range todo {
			wg.Add(1)
			go func(i int, h hosts.HostInfo) {
				defer wg.Done()
				results[i] = d.probe(ctx, h, upload)
			}(i, h)
		}
		wg.Wait()
		for i, h := range todo {
			state[h.PublicKey] = results[i]
		}
		if !firstRound {
			unreachableRetried = true
		}

		reachable, funded := 0, 0
		for _, h := range candidates {
			p := state[h.PublicKey]
			if p.Reachable {
				reachable++
			}
			if p.Funded {
				funded++
			}
		}
		base := readinessError{Reachable: reachable, Funded: funded, Needed: needed, Total: len(candidates)}

		if funded >= needed {
			return nil
		}
		if reachable < needed && (unreachableRetried || len(candidates) < needed) {
			e := base
			e.Code = readinessCodeNetwork
			e.Message = networkMessage(reachable, needed, len(candidates), upload)
			return &e
		}
		if reachable >= needed && d.now().Sub(started) >= d.balanceGrace {
			if bal, ok := d.balance(); ok && bal <= 0 {
				e := base
				e.Code = readinessCodeBalance
				e.Message = fmt.Sprintf("insufficient balance: your account balance is $0.00 and only %d of the %d hosts needed are funded. "+
					"Add funds with 'tessera topup 5' (crypto minimum ~$0.38 in USDC on Base; live list: 'tessera topup --minimums'), then retry.", funded, needed)
				return &e
			}
		}
		if funded > lastProgressFunded {
			lastProgressFunded, lastProgressAt = funded, d.now()
		}
		stalled := reachable >= needed && d.stallWait > 0 && d.now().Sub(lastProgressAt) >= d.stallWait
		if d.now().After(deadline) || stalled || (d.maxRounds > 0 && round >= d.maxRounds) {
			e := base
			e.Code = readinessCodeNotReady
			e.Message = fmt.Sprintf("account not ready yet: only %d of the %d hosts needed are funded for this account after waiting %s. "+
				"Funding normally lands within seconds; retry in a minute, and check 'tessera account'.",
				funded, needed, d.now().Sub(started).Round(time.Second))
			return &e
		}
		if reachable >= needed && funded != lastFunded && d.progress != nil {
			d.progress(funded, needed)
			lastFunded = funded
		}
		firstRound = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		d.sleep(readinessPollInterval)
	}
}

func networkMessage(reachable, needed, total int, upload bool) string {
	op := "upload"
	if !upload {
		op = "download"
	}
	if total < needed {
		return fmt.Sprintf("only %d storage hosts are currently usable, and each %s needs %d. Retry later.", total, op, needed)
	}
	return fmt.Sprintf("only %d of the %d needed hosts are reachable from your network (%d hosts probed). "+
		"Each %s needs %d hosts reachable over direct TCP (siamux, on the port each host announces: usually 9984, some hosts use others such as 9994 or 9974). "+
		"Something on this network (a firewall, VPN, proxy or sandbox) is blocking direct TCP to the rest. "+
		"Allow outbound TCP to the storage hosts, or run from a network that allows it; "+
		"'tessera account --json' and https://tessera.storage/docs/errors explain more.",
		reachable, needed, total, op, needed)
}

// requireTransferReady runs the readiness gate before an upload or download
// and exits with the classified message when the account cannot transfer.
func requireTransferReady(ctx context.Context, cfg *Config, upload bool) {
	requireTransferReadyFor(ctx, cfg, upload, "")
}

// requireTransferReadyFor is requireTransferReady scoped to one stored object:
// a download only needs the hosts that hold that object's shards, so it probes
// those (GET /objects/{id}/hosts) rather than every listed host. The general
// host list is still fetched each round because that is what triggers the
// free-path funding.
func requireTransferReadyFor(ctx context.Context, cfg *Config, upload bool, objectID string) {
	if os.Getenv("TESSERA_SKIP_PREFLIGHT") == "1" {
		return
	}
	needed := dataShards
	if upload {
		needed = dataShards + parityShards
	}
	d, err := newReadinessDeps(cfg)
	if err != nil {
		fatal("%v", err)
	}
	if !upload && objectID != "" {
		if lister := objectHostLister(cfg, objectID, d.listHosts); lister != nil {
			d.listHosts = lister
		}
	}
	transferProgress.Phase("checking host funding")
	err = checkReadiness(ctx, d, needed, upload)
	transferProgress.Finish()
	if err != nil {
		fatal("%v", err)
	}
}

// isHostShortageError reports whether err is the SDK's raw host-shortage
// failure, which must never reach the user unexplained.
func isHostShortageError(err error) bool {
	if err == nil {
		return false
	}
	var re *readinessError
	if errors.As(err, &re) {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no more hosts") || strings.Contains(s, "not enough shards") ||
		strings.Contains(s, "not enough hosts") || strings.Contains(s, "too few shards")
}

// explainTransferError replaces a raw host-shortage error with its classified
// cause (one fresh probe, no waiting). Other errors pass through unchanged.
func explainTransferError(cfg *Config, err error, upload bool) error {
	if !isHostShortageError(err) {
		return err
	}
	needed := dataShards
	if upload {
		needed = dataShards + parityShards
	}
	d, derr := newReadinessDeps(cfg)
	if derr != nil {
		return fmt.Errorf("not enough storage hosts could be used for this transfer (need %d)", needed)
	}
	d.sleep = func(time.Duration) {}
	d.maxRounds = 2
	d.balanceGrace = 0
	d.progress = nil
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if cerr := checkReadiness(ctx, d, needed, upload); cerr != nil {
		return cerr
	}
	return fmt.Errorf("not enough storage hosts accepted this transfer (need %d). The hosts look ready now; retry", needed)
}

// appKeyPublicHex returns the hex public key for a stored app key, or "" when
// the key is malformed.
func appKeyPublicHex(appKeyHex string) string {
	b, err := hex.DecodeString(appKeyHex)
	if err != nil || len(b) != 64 {
		return ""
	}
	pk := types.PrivateKey(b).PublicKey()
	return hex.EncodeToString(pk[:])
}

var (
	syncShortageMu    sync.Mutex
	syncShortageCache = map[bool]error{}
)

// explainSyncTransferError maps a host-shortage error from a sync transfer to
// its classified cause, probing at most once per direction per process.
func explainSyncTransferError(err error, upload bool) error {
	if !isHostShortageError(err) {
		return err
	}
	syncShortageMu.Lock()
	defer syncShortageMu.Unlock()
	if cached, ok := syncShortageCache[upload]; ok {
		return cached
	}
	cfg, cerr := loadConfig()
	var out error
	if cerr != nil {
		out = fmt.Errorf("not enough storage hosts could be used for this transfer")
	} else {
		out = explainTransferError(cfg, err, upload)
	}
	syncShortageCache[upload] = out
	return out
}

// explainFatalHostShortage is the last line of defence in fatal(): a raw SDK
// host-shortage message is replaced by its classified cause.
func explainFatalHostShortage(msg string) string {
	if !isHostShortageError(errors.New(msg)) {
		return msg
	}
	upload := !strings.HasPrefix(strings.ToLower(msg), "download")
	return explainSyncTransferError(errors.New(msg), upload).Error()
}

// objectHostLister returns a host lister that triggers funding via the general
// host list and then returns the hosts storing objectID. Falls back to the
// general list when the object's hosts cannot be fetched.
var objectHostLister = func(cfg *Config, objectID string, general func(context.Context) ([]hosts.HostInfo, error)) func(context.Context) ([]hosts.HostInfo, error) {
	id, err := parseObjectID(objectID)
	if err != nil {
		return nil
	}
	client, appKey, err := connectAPI(cfg)
	if err != nil {
		return nil
	}
	return func(ctx context.Context) ([]hosts.HostInfo, error) {
		all, gerr := general(ctx)
		obj, oerr := client.ObjectHosts(ctx, appKey, id)
		if oerr != nil || len(obj) == 0 {
			return all, gerr
		}
		return obj, nil
	}
}
