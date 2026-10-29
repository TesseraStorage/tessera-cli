package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/indexd/hosts"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }
func fakeHosts(n int) []hosts.HostInfo {
	out := make([]hosts.HostInfo, n)
	for i := range out {
		out[i] = hosts.HostInfo{PublicKey: types.PublicKey{byte(i), byte(i >> 8), 1}, GoodForUpload: true}
	}
	return out
}

// testDeps: total hosts, reachable count, funded(round) returns how many
// reachable hosts are funded at a given probe round.
func testDeps(total, reachable int, funded func(round int) int, bal float64, balKnown bool) (readinessDeps, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	list := fakeHosts(total)
	index := map[types.PublicKey]int{}
	for i, h := range list {
		index[h.PublicKey] = i
	}
	round := 0
	d := readinessDeps{
		listHosts: func(context.Context) ([]hosts.HostInfo, error) { round++; return list, nil },
		probe: func(_ context.Context, h hosts.HostInfo, _ bool) hostProbe {
			i := index[h.PublicKey]
			if i >= reachable {
				return hostProbe{}
			}
			return hostProbe{Reachable: true, Funded: i < funded(round)}
		},
		balance:      func() (float64, bool) { return bal, balKnown },
		sleep:        clk.sleep,
		now:          clk.now,
		balanceGrace: readinessBalanceGrace,
	}
	return d, clk
}

func readinessCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var re *readinessError
	if !errors.As(err, &re) {
		t.Fatalf("not a readinessError: %v", err)
	}
	return re.Code
}

func TestReadinessReadyImmediately(t *testing.T) {
	d, _ := testDeps(37, 37, func(int) int { return 37 }, 0.05, true)
	if err := checkReadiness(context.Background(), d, 30, true); err != nil {
		t.Fatal(err)
	}
}

func TestReadinessWaitsForFreePathFunding(t *testing.T) {
	// $0 balance and nothing funded at first: the free-path promo and host
	// funding land on the third round. Must wait, not claim "no balance".
	waits := 0
	d, _ := testDeps(37, 37, func(r int) int {
		if r >= 3 {
			return 37
		}
		return 0
	}, 0, true)
	d.progress = func(int, int) { waits++ }
	if err := checkReadiness(context.Background(), d, 30, true); err != nil {
		t.Fatal(err)
	}
	if waits == 0 {
		t.Fatal("expected a waiting message")
	}
}

func TestReadinessNetworkBlocked(t *testing.T) {
	d, _ := testDeps(37, 15, func(int) int { return 15 }, 0.05, true)
	err := checkReadiness(context.Background(), d, 30, true)
	if readinessCode(t, err) != readinessCodeNetwork {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "only 15 of the 30 needed hosts are reachable") {
		t.Fatalf("message: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "no more hosts") {
		t.Fatal("raw SDK text leaked")
	}
}

func TestReadinessDownloadNeedsTen(t *testing.T) {
	d, _ := testDeps(37, 15, func(int) int { return 15 }, 0.05, true)
	if err := checkReadiness(context.Background(), d, 10, false); err != nil {
		t.Fatal(err)
	}
}

func TestReadinessInsufficientBalanceAfterGrace(t *testing.T) {
	d, _ := testDeps(37, 37, func(int) int { return 5 }, 0, true)
	err := checkReadiness(context.Background(), d, 30, true)
	if readinessCode(t, err) != readinessCodeBalance {
		t.Fatalf("got %v", err)
	}
}

func TestReadinessUnknownBalanceTimesOut(t *testing.T) {
	d, clk := testDeps(37, 37, func(int) int { return 5 }, 0, false)
	start := clk.t
	err := checkReadiness(context.Background(), d, 30, true)
	if readinessCode(t, err) != readinessCodeNotReady {
		t.Fatalf("got %v", err)
	}
	if clk.t.Sub(start) < readinessMaxWait {
		t.Fatalf("gave up after %s, want >= %s", clk.t.Sub(start), readinessMaxWait)
	}
}

func TestReadinessExplainModeIsBounded(t *testing.T) {
	d, clk := testDeps(37, 37, func(int) int { return 5 }, 0.05, true)
	d.maxRounds = 2
	d.balanceGrace = 0
	start := clk.t
	err := checkReadiness(context.Background(), d, 30, true)
	if readinessCode(t, err) != readinessCodeNotReady {
		t.Fatalf("got %v", err)
	}
	if clk.t.Sub(start) > 2*readinessPollInterval {
		t.Fatalf("explain mode waited %s", clk.t.Sub(start))
	}
}

func TestIsHostShortageError(t *testing.T) {
	for _, s := range []string{"failed to upload: no more hosts available", "not enough shards: 12/30"} {
		if !isHostShortageError(errors.New(s)) {
			t.Fatalf("%q not detected", s)
		}
	}
	if isHostShortageError(errors.New("object not found")) {
		t.Fatal("false positive")
	}
	if isHostShortageError(&readinessError{Message: "only 1 of the 30 needed hosts... no more hosts"}) {
		t.Fatal("classified errors must not be re-classified")
	}
}

func TestSignupHintStatesRealMinimums(t *testing.T) {
	// guard for the old 'tessera topup 10' hint
	out := captureStdout(t, func() { printSignupResult(signupResult{AccountID: "AGNT-test", APIKey: "test-key"}, false) })
	if strings.Contains(out, "topup 10") || strings.Contains(out, "minimum $0.01") || !strings.Contains(out, "$0.38") || !strings.Contains(out, "topup --minimums") {
		t.Fatalf("signup hint: %q", out)
	}
}

func TestReadyNoteWording(t *testing.T) {
	n := readyNote(false)
	if strings.Contains(n, "does not block") || !strings.Contains(n, "first top-up") {
		t.Fatalf("ready note: %q", n)
	}
}
