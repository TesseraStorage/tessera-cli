package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	siastorage "go.sia.tech/siastorage"
)

// pinTimeout bounds the pinning step of an upload (the SDK's PinSlabs
// batches plus the final PinObject request). Uploads run under a 6-hour
// overall deadline; without a pin-specific bound a stuck pin request on a busy
// server would sit silently for minutes. The server caps its own pin
// transaction at the same 45s, so a healthy pin finishes well inside it.
const pinTimeout = 45 * time.Second

// pinSlabBatch mirrors the SDK's PinSlabs batch size (siastorage pinBatchSize).
// Very large objects need several PinSlabs requests, so the bound is 45s per
// batch rather than 45s for the whole object.
const pinSlabBatch = 50

// pinBudget returns the pin deadline for an object with the given number of
// slabs: pinTimeout per PinSlabs batch, and never less than one pinTimeout.
func pinBudget(numSlabs int) time.Duration {
	batches := (numSlabs + pinSlabBatch - 1) / pinSlabBatch
	if batches < 1 {
		batches = 1
	}
	return time.Duration(batches) * pinTimeout
}

// pinContext derives the pin step's context from the upload context: it keeps
// the caller's cancellation and overall deadline and adds the pin bound.
func pinContext(ctx context.Context, numSlabs int) (context.Context, context.CancelFunc, time.Duration) {
	budget := pinBudget(numSlabs)
	pctx, cancel := context.WithTimeout(ctx, budget)
	return pctx, cancel, budget
}

// errPinTimeout marks a pin that ran out of its own time bound.
var errPinTimeout = errors.New("pinning timed out")

// pinTimeoutError rewrites err into a clear message when the pin context hit
// its own deadline while the outer upload context was still live. Any other
// error (including the outer 6h deadline or Ctrl-C) is returned unchanged.
func pinTimeoutError(ctx, pctx context.Context, budget time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s: the Tessera server is busy, please retry (%v)", errPinTimeout, budget.Round(time.Second), err)
	}
	return err
}

// pinObject pins obj under the pin-step bound (see pinTimeout).
func (r *Remote) pinObject(ctx context.Context, obj siastorage.Object) error {
	pctx, cancel, budget := pinContext(ctx, len(obj.Slabs()))
	defer cancel()
	return pinTimeoutError(ctx, pctx, budget, r.sdk.PinObject(pctx, obj))
}
