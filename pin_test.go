package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPinContextCapsLongUploadDeadline(t *testing.T) {
	outer, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	pctx, pcancel, budget := pinContext(outer, 3)
	defer pcancel()
	if budget != pinTimeout || pinTimeout != 45*time.Second {
		t.Fatalf("budget = %v, want 45s", budget)
	}
	dl, ok := pctx.Deadline()
	if !ok {
		t.Fatal("pin context has no deadline")
	}
	if left := time.Until(dl); left <= 0 || left > 45*time.Second {
		t.Fatalf("pin deadline %v away, want (0, 45s]", left)
	}
	// The overall upload deadline is untouched.
	if odl, _ := outer.Deadline(); time.Until(odl) < 5*time.Hour {
		t.Fatalf("outer deadline changed: %v left", time.Until(odl))
	}
}

func TestPinContextKeepsShorterOuterDeadline(t *testing.T) {
	outer, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pctx, pcancel, _ := pinContext(outer, 1)
	defer pcancel()
	dl, _ := pctx.Deadline()
	if left := time.Until(dl); left > 5*time.Second {
		t.Fatalf("pin deadline %v away, want <= outer 5s", left)
	}
}

func TestPinContextBackgroundIsBounded(t *testing.T) {
	pctx, pcancel, _ := pinContext(context.Background(), 0)
	defer pcancel()
	dl, ok := pctx.Deadline()
	if !ok || time.Until(dl) > 45*time.Second {
		t.Fatalf("background pin context not bounded to 45s (ok=%v)", ok)
	}
}

func TestPinBudgetPerBatch(t *testing.T) {
	cases := map[int]time.Duration{0: 45 * time.Second, 1: 45 * time.Second, 50: 45 * time.Second, 51: 90 * time.Second, 120: 135 * time.Second}
	for n, want := range cases {
		if got := pinBudget(n); got != want {
			t.Errorf("pinBudget(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestPinTimeoutErrorIsClear(t *testing.T) {
	outer, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	pctx, pcancel := context.WithTimeout(outer, time.Millisecond)
	defer pcancel()
	<-pctx.Done()
	err := pinTimeoutError(outer, pctx, pinTimeout, errors.New("failed to pin slabs: context deadline exceeded"))
	if !errors.Is(err, errPinTimeout) {
		t.Fatalf("err = %v, want errPinTimeout", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "pinning timed out after 45s") || !strings.Contains(msg, "busy, please retry") {
		t.Fatalf("unclear message: %q", msg)
	}
}

func TestPinTimeoutErrorLeavesOtherErrorsAlone(t *testing.T) {
	base := errors.New("boom")
	if err := pinTimeoutError(context.Background(), context.Background(), pinTimeout, base); err != base {
		t.Fatalf("non-timeout error rewritten: %v", err)
	}
	// Outer cancellation (Ctrl-C / 6h deadline) is not reported as a pin timeout.
	outer, cancel := context.WithCancel(context.Background())
	pctx, pcancel := context.WithTimeout(outer, time.Millisecond)
	defer pcancel()
	<-pctx.Done()
	cancel()
	if err := pinTimeoutError(outer, pctx, pinTimeout, base); err != base {
		t.Fatalf("outer cancel rewritten: %v", err)
	}
	if pinTimeoutError(outer, pctx, pinTimeout, nil) != nil {
		t.Fatal("nil error rewritten")
	}
}
