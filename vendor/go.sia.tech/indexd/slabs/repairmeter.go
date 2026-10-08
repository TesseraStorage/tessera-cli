package slabs

import (
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/rhp/v4"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// REPAIR COST RECORDER — a decorator on HostClient, used ONLY by the Migrator.
//
// Skeen: "We don't need a full time 'listener', simply a recorder of the repair actions." And on the
// fork: "not in a way that changes it's actual operation, rather in a way that simply reports its
// actions to us for recording/cost attribution."
//
// Both constraints are structural here, not aspirational:
//
//   NOT A LISTENER. There is no daemon, no goroutine, no subscription, no polling and nothing to
//   supervise. This is a pass-through on a call that is ALREADY HAPPENING. When no repair is
//   running, nothing in this file executes -- so it cannot go stale, cannot time out, and cannot
//   silently freeze the way the rotation pool did.
//
//   DOES NOT CHANGE OPERATION. Every method forwards to the wrapped client and returns its result
//   and error UNCHANGED. The recorder observes the return value on the way past; it never
//   substitutes, retries, blocks or reorders anything.
//
// WHY EMBEDDING THE INTERFACE IS THE SAFETY PROPERTY: `HostClient` is embedded, so any method we do
// not explicitly override is forwarded automatically. If upstream adds a method to HostClient, this
// decorator keeps compiling and keeps forwarding correctly instead of silently dropping behaviour.
// Only the two RPCs that cost money are overridden.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// RepairEvent is one priced RPC in a repair sequence. Currencies stay as types.Currency here and are
// converted to exact decimal strings at the sink -- never through a float.
type RepairEvent struct {
	SequenceID string
	// Cause is the comma-joined set of causes (lost, bad_host, bad_contract) among the current
	// sequence's required shards -- a SET because egress RPCs can serve reconstruction of
	// multiple shards with different causes at once (see
	// .hermes/plans/2026-08-23_repair-events-cause-column.md §2-3).
	Cause      string
	Direction  string // "egress" (ReadSector) or "ingress" (WriteSector)
	HostKey    types.PublicKey
	ShardRoot  string
	Usage      proto.Usage
	Bytes      uint64
	Elapsed    time.Duration
	OK         bool
	Err        string
	// Note records HOW the figure was obtained when it was not the client's own: on a failed RPC
	// the client returns an empty Usage even though the host already debited, so the amount is
	// reconstructed from the host's price schedule and said so here. An unexplained number is a
	// number nobody can audit.
	Note string
}

// RepairRecorder receives one event per priced RPC.
//
// IT MUST NOT BLOCK. Recording is subordinate to the repair, always: a full buffer must drop the
// event and count the drop, never slow a migration down. This is the one place where losing data is
// the correct trade, because the alternative is letting bookkeeping interfere with the operation it
// is only observing.
type RepairRecorder interface {
	RecordRepair(RepairEvent)
}

// newRepairSequenceID mints an id for one repair of one slab.
//
// Slab id ALONE is not enough: the same slab is repaired repeatedly over its life (a slab with 2,778
// due repairs would collapse into one "sequence" and its cost-per-repair would read as the lifetime
// total). So the id is slab + a monotonic counter, which keeps successive repairs of the same slab
// distinct while still naming the slab in the row.
func newRepairSequenceID(slabID SlabID) string {
	n := repairSeqCounter.Add(1)
	return slabID.String()[:16] + "-" + strconv.FormatUint(n, 10)
}

var repairSeqCounter atomic.Uint64

// repairMeter decorates a HostClient for the Migrator only.
type repairMeter struct {
	HostClient // embedded: everything not overridden below forwards unchanged

	rec RepairRecorder

	// seq identifies the current repair sequence. The Migrator repairs one slab at a time through
	// executeMigration, so a monotonic counter stamped per sequence is enough to group the ~11
	// participants without threading a context value through upstream code we do not want to touch.
	seq atomic.Pointer[string]

	// causes holds the comma-joined set of DISTINCT causes among the current sequence's required
	// shards (see .hermes/plans/2026-08-23_repair-events-cause-column.md §2-3: a sequence can
	// contain shards with different causes sharing the same egress RPCs, so this is a SET, never
	// a single arbitrarily-picked value). Same lifetime as seq.
	causes atomic.Pointer[string]
}

// WithRepairRecorder wraps the Migrator's HostClient so every ReadSector/WriteSector it performs is
// priced. Returns the client unchanged when rec is nil, so the recorder is opt-in and its absence
// costs nothing.
func WithRepairRecorder(hosts HostClient, rec RepairRecorder) HostClient {
	if rec == nil {
		return hosts
	}
	m := &repairMeter{HostClient: hosts, rec: rec}
	empty := ""
	m.seq.Store(&empty)
	m.causes.Store(&empty)
	return m
}

// setSequence stamps subsequent RPCs with a sequence id and the comma-joined cause set for that
// sequence. Called by the Migrator at the start of each slab migration.
func (m *repairMeter) setSequence(id string, causes string) {
	m.seq.Store(&id)
	m.causes.Store(&causes)
}

func (m *repairMeter) sequence() string {
	if p := m.seq.Load(); p != nil {
		return *p
	}
	return ""
}

func (m *repairMeter) causeSet() string {
	if p := m.causes.Load(); p != nil {
		return *p
	}
	return ""
}

// chargedUsage returns what the HOST actually debited, which is not always what the client returns.
//
// THE DEFECT THIS FIXES (found 2026-08-18 after Grok Q4 pointed at the host implementation):
// `coreutils/rhp/v4/server.go handleRPCReadSector` calls
// `DebitAccount(prices.RPCReadSectorCost(req.Length))` at line 211, SIX LINES BEFORE it reads the
// sector from disk at line 217. The money is gone the moment the host accepts the request, and there
// is no refund path in RHP4 -- the only Credit* functions are account funding.
//
// But the CLIENT (`coreutils/rhp/v4/rpc.go RPCReadSector`) returns `RPCReadSectorResult{}` -- an
// EMPTY Usage -- on every error path: dial failure, short read, bad proof, context cancellation.
// So recording `res.Usage` verbatim writes ZERO for an RPC WE WERE CHARGED FOR IN FULL.
//
// That is the exact shape of the pass-over bug that already bit us twice: the failure is invisible
// in the data and reads as "nothing happened". Worse here, because repair RACES and then CANCELS the
// losers -- so the systematically-missing rows would be precisely the wasted reads, i.e. the entire
// cost of racing. Cancelled reads are the thing we most need to see.
//
// So on failure we RECONSTRUCT the debit using the same function the host used. Priced from the
// host's own current price schedule; if that is unavailable we return the empty usage and the row's
// `note` says so, because an invented price is worse than an admitted gap.
func (m *repairMeter) chargedUsage(ctx context.Context, hostKey types.PublicKey, res proto.Usage,
	err error, length uint64, write bool) (proto.Usage, string) {

	if err == nil {
		return res, "" // the client's own figure, authoritative on success
	}
	// A failed RPC still cost us. Recover the price the host charged.
	prices, perr := m.HostClient.Prices(ctx, hostKey)
	if perr != nil {
		return res, "charged but unpriced: host price schedule unavailable: " + perr.Error()
	}
	if write {
		return prices.RPCWriteSectorCost(length), "reconstructed from host prices (failed RPC, host debits up front)"
	}
	return prices.RPCReadSectorCost(length), "reconstructed from host prices (failed RPC, host debits up front)"
}

// ReadSector — the EGRESS side. ~10 of these per repaired shard, and the expensive direction:
// measured 8.5x ingress on live client traffic. This is where repair's cost actually lives.
func (m *repairMeter) ReadSector(ctx context.Context, accountKey types.PrivateKey, hostKey types.PublicKey,
	root types.Hash256, w io.Writer, offset, length uint64) (rhp.RPCReadSectorResult, error) {

	start := time.Now()
	res, err := m.HostClient.ReadSector(ctx, accountKey, hostKey, root, w, offset, length)

	// RECORDED WHETHER OR NOT IT SUCCEEDED: a failed RPC can still have cost money for work the
	// host did before the error. `ok` distinguishes them so "what did we pay" and "what did we get"
	// stay separate questions.
	usage, note := m.chargedUsage(ctx, hostKey, res.Usage, err, length, false)
	ev := RepairEvent{
		SequenceID: m.sequence(),
		Cause:      m.causeSet(),
		Direction:  "egress",
		HostKey:    hostKey,
		ShardRoot:  root.String(),
		Usage:      usage,
		Bytes:      length,
		Elapsed:    time.Since(start),
		OK:         err == nil,
		Note:       note,
	}
	if err != nil {
		ev.Err = err.Error()
	}
	m.rec.RecordRepair(ev)

	return res, err // unchanged
}

// WriteSector — the INGRESS side. Exactly 1 per repaired shard: the rebuilt shard being placed.
func (m *repairMeter) WriteSector(ctx context.Context, accountKey types.PrivateKey, hostKey types.PublicKey,
	data []byte) (rhp.RPCWriteSectorResult, error) {

	start := time.Now()
	res, err := m.HostClient.WriteSector(ctx, accountKey, hostKey, data)

	usage, note := m.chargedUsage(ctx, hostKey, res.Usage, err, uint64(len(data)), true)
	ev := RepairEvent{
		SequenceID: m.sequence(),
		Cause:      m.causeSet(),
		Direction:  "ingress",
		HostKey:    hostKey,
		Usage:      usage,
		Bytes:      uint64(len(data)),
		Elapsed:    time.Since(start),
		OK:         err == nil,
		Note:       note,
	}
	if err == nil {
		ev.ShardRoot = res.Root.String()
	} else {
		ev.Err = err.Error()
	}
	m.rec.RecordRepair(ev)

	return res, err // unchanged
}
