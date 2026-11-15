package siastorage

import (
	"fmt"
	"strings"
	"time"

	"go.sia.tech/core/types"
)

// Sonnet packet "error-transparency" (2026-11-15): every fatal upload/
// download error must carry the per-host detail already computed at the
// failure site -- host key, underlying error, elapsed -- instead of a
// generic string like ErrNoMoreHosts on its own.

// maxAttemptRing bounds how many recent failed per-host attempts are kept
// for a single shard/sector's host loop.
const maxAttemptRing = 5

// maxAttemptErrLen caps how much of one underlying error's text is kept,
// so a single verbose host error cannot dominate the summary.
const maxAttemptErrLen = 120

// attemptRecord is one failed per-host attempt kept for the error ring.
type attemptRecord struct {
	host    types.PublicKey
	err     error
	elapsed time.Duration
}

// attemptRing keeps the last maxAttemptRing failed attempts for a single
// shard/sector's host loop, oldest first, and formats them onto the
// terminal error for that shard/sector. Not safe for concurrent use --
// every caller in this package only appends from the single goroutine that
// owns the attempt loop (results already arrive serialized over a channel).
type attemptRing struct {
	entries []attemptRecord
}

// add records one failed attempt. A nil err is never a failure and is
// ignored, so callers can call this unconditionally on every result.
func (r *attemptRing) add(host types.PublicKey, err error, elapsed time.Duration) {
	if err == nil {
		return
	}
	r.entries = append(r.entries, attemptRecord{host, err, elapsed})
	if len(r.entries) > maxAttemptRing {
		r.entries = r.entries[len(r.entries)-maxAttemptRing:]
	}
}

// wrap attaches the ring's summary to err via %w, so errors.Is/As still see
// through to the original sentinel (ErrNoMoreHosts, ErrNotEnoughShards, a
// context error, ...). Returns err unchanged if there is nothing recorded
// yet (e.g. the very first host pick failed before any attempt ran).
func (r *attemptRing) wrap(err error) error {
	if err == nil || len(r.entries) == 0 {
		return err
	}
	parts := make([]string, len(r.entries))
	for i, e := range r.entries {
		parts[i] = fmt.Sprintf("%s %s %s", hostPrefix(e.host), truncateAttemptErr(e.err), formatAttemptElapsed(e.elapsed))
	}
	return fmt.Errorf("%w (last attempts: %s)", err, strings.Join(parts, "; "))
}

// hostPrefix renders a host key the same short way everywhere: "ed25519:"
// plus the first 12 hex chars, with "…" when truncated.
func hostPrefix(pk types.PublicKey) string {
	s := pk.String()
	const n = len("ed25519:") + 12
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func truncateAttemptErr(err error) string {
	s := err.Error()
	if len(s) <= maxAttemptErrLen {
		return s
	}
	return s[:maxAttemptErrLen] + "…"
}

func formatAttemptElapsed(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}
