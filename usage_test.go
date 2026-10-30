package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakeUsageAPI implements billingAPI with only Account wired. The embedded nil
// interface satisfies the other methods and panics if one is called, which is
// part of the assertion that `usage` speaks only to GET /account.
type fakeUsageAPI struct {
	billingAPI
	calls int
	acct  map[string]any
	err   error
}

func (f *fakeUsageAPI) Account() (map[string]any, error) {
	f.calls++
	return f.acct, f.err
}

// captureOutput runs f with stdout and stderr redirected to pipes and returns
// both streams. The usage --cost hint goes to stderr so --json stdout stays
// parseable, so tests need to tell the streams apart.
func captureOutput(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	doneOut := make(chan string, 1)
	doneErr := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(rOut)
		doneOut <- string(data)
	}()
	go func() {
		data, _ := io.ReadAll(rErr)
		doneErr <- string(data)
	}()
	f()
	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-doneOut, <-doneErr
}

// usageTestIndex is a small two-file index used by the formatting tests. The
// sync-root folder-name cases are covered by TestCollectUsageStorage.
func usageTestIndex() *Index {
	return &Index{
		Schema: indexSchema,
		Paths: map[string]*IndexEntry{
			"photos/a.jpg": {Path: "photos/a.jpg", Size: 1024},
			"photos/b.jpg": {Path: "photos/b.jpg", Size: 512},
		},
	}
}

// usageTestStorageHuman is the exact human storage output for usageTestIndex
// without a billing credential. It reports only what this machine knows: the
// synced files and their logical sizes. Network size is never estimated here;
// a billing credential adds the backend's real network_bytes.
const usageTestStorageHuman = "Storage usage\n" +
	"\n" +
	"  files:           2\n" +
	"  your files:      1.5 KiB (logical size, as uploaded from this machine)\n" +
	"\n" +
	"  Note: Sia costs depend on your contracts and current host prices.\n" +
	"  Run 'tessera status' for account readiness.\n"

// usageTestStorageJSON is the exact --json storage output for usageTestIndex
// without a billing credential (keys sorted by encoding/json).
const usageTestStorageJSON = `{
  "files": 2,
  "folders": [
    {
      "folder": "photos",
      "files": 2,
      "bytes": 1536
    }
  ],
  "logical_bytes": 1536
}
`

// TestUsageStorageHumanWithoutBilling pins the local-only storage output: with
// no billing credential it reports the synced files and logical sizes, with no
// network estimate, and writes nothing to stderr.
func TestUsageStorageHumanWithoutBilling(t *testing.T) {
	stdout, stderr := captureOutput(t, func() {
		usageReport(nil, &Config{IndexerURL: defaultIndexer}, usageTestIndex())
	})
	if stdout != usageTestStorageHuman {
		t.Fatalf("usage output changed:\n got:\n%q\nwant:\n%q", stdout, usageTestStorageHuman)
	}
	if stderr != "" {
		t.Fatalf("usage without billing config wrote to stderr: %q", stderr)
	}
}

// TestUsageStorageByFolder keeps the --by-folder table layout intact.
func TestUsageStorageByFolder(t *testing.T) {
	stdout, _ := captureOutput(t, func() {
		usageReport([]string{"--by-folder"}, &Config{IndexerURL: defaultIndexer}, usageTestIndex())
	})
	want := "Storage usage\n" +
		"\n" +
		"  files:           2\n" +
		"  your files:      1.5 KiB (logical size, as uploaded from this machine)\n" +
		"\n" +
		"  " + fmt.Sprintf("%-40s  %8s  %12s", "FOLDER", "FILES", "SIZE") + "\n" +
		"  " + strings.Repeat("-", 64) + "\n" +
		"  " + fmt.Sprintf("%-40s  %8d  %12s", "photos", 2, "1.5 KiB") + "\n" +
		"\n" +
		"  Note: Sia costs depend on your contracts and current host prices.\n" +
		"  Run 'tessera status' for account readiness.\n"
	if stdout != want {
		t.Fatalf("usage --by-folder output changed:\n got:\n%q\nwant:\n%q", stdout, want)
	}
}

// TestUsageStorageJSONWithoutBilling pins --json without a billing credential:
// the local storage keys only, no fabricated network estimate and no billing
// key, nothing on stderr.
func TestUsageStorageJSONWithoutBilling(t *testing.T) {
	stdout, stderr := captureOutput(t, func() {
		usageReport([]string{"--json"}, &Config{IndexerURL: defaultIndexer}, usageTestIndex())
	})
	if stdout != usageTestStorageJSON {
		t.Fatalf("usage --json output changed:\n got:\n%q\nwant:\n%q", stdout, usageTestStorageJSON)
	}
	if stderr != "" {
		t.Fatalf("usage --json wrote to stderr: %q", stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("usage --json is not JSON: %v", err)
	}
	if _, ok := got["billing"]; ok {
		t.Fatalf("billing key present without a billing credential: %#v", got)
	}
}

// TestCollectUsageStorage buckets by the owning sync root's remote prefix, the
// path's first segment, or an em dash, keeps the per-folder counts, and sums
// the total.
func TestCollectUsageStorage(t *testing.T) {
	testConfigHome(t)
	if err := saveSyncConfig(&SyncConfig{ID: "root-1", RemotePrefix: "media"}); err != nil {
		t.Fatal(err)
	}
	idx := &Index{Paths: map[string]*IndexEntry{
		"a": {Path: "media/a", Root: "root-1", Size: 10},
		"b": {Path: "docs/b", Root: "root-2", Size: 20},
		"c": {Path: "readme", Size: 30},
	}}
	byFolder, total := collectUsageStorage(idx)
	want := map[string]usageFolder{
		"media":  {Files: 1, Bytes: 10},
		"root-2": {Files: 1, Bytes: 20},
		"\u2014": {Files: 1, Bytes: 30},
	}
	if !reflect.DeepEqual(byFolder, want) {
		t.Fatalf("byFolder = %#v, want %#v", byFolder, want)
	}
	if total != 60 {
		t.Fatalf("total = %d, want 60", total)
	}
}

// TestEgressEstimateUSD pins the price card arithmetic: billable = max(0,
// used − allowance) at $5 per decimal TB (1e12 bytes, not 2^40), rounded to
// cents.
func TestEgressEstimateUSD(t *testing.T) {
	cases := []struct {
		name      string
		used      uint64
		allowance uint64
		want      string
	}{
		{"allowance netted first", 2.5e12, 0.5e12, "10.00"},
		{"allowance equals used", 1e12, 1e12, "0.00"},
		{"allowance above used", 1e12, 2e12, "0.00"},
		{"zero usage", 0, 0, "0.00"},
		{"sub-cent usage rounds to zero", 1, 0, "0.00"},
		{"fractional cents round to the nearest cent", 123456789000, 0, "0.62"},
		{"decimal TB, not TiB", 1099511627776, 0, "5.50"},
		{"exactly one decimal TB", 1e12, 0, "5.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := egressEstimateUSD(tc.used, tc.allowance); got != tc.want {
				t.Fatalf("egressEstimateUSD(%d, %d) = %q, want %q", tc.used, tc.allowance, got, tc.want)
			}
		})
	}
}

// TestUsageBillingSectionHuman: with a billing credential the storage block is
// followed by the billing section (network size from GET /account, open slabs,
// egress used of allowance, balance, period end, days left) and no estimate
// unless --cost was asked for. Network size must be the backend fixture's real
// value, not a client-side estimate.
func TestUsageBillingSectionHuman(t *testing.T) {
	testConfigHome(t)
	fake := &fakeUsageAPI{acct: accountObject(t, accountServerJSON)}
	stubBillingClient(t, fake)
	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingSession:  "sess-live",
		BillingAgentKey: "tsk_secret",
	}

	stdout, stderr := captureOutput(t, func() { usageReport(nil, cfg, usageTestIndex()) })

	if fake.calls != 1 {
		t.Fatalf("Account calls = %d, want 1", fake.calls)
	}
	if stderr != "" {
		t.Fatalf("usage with billing config wrote to stderr: %q", stderr)
	}
	if !strings.HasPrefix(stdout, usageTestStorageHuman) {
		t.Fatalf("billing output no longer starts with the storage block:\n%s", stdout)
	}
	if strings.Contains(stdout, "network (\u2248") {
		t.Errorf("client-side network estimate still printed:\n%s", stdout)
	}
	for _, want := range []string{
		"Billing",
		"network size",
		"360.00 MiB",
		"3 slabs of 40 MiB",
		"3× redundancy",
		"downloads",
		"measured host spend",
		"Why the sizes differ",
		"balance",
		"$7.50",
		"days left",
		"29",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("billing section missing %q:\n%s", want, stdout)
		}
	}
	if line := lineWithPrefix(stdout, "period ends"); !strings.HasSuffix(strings.TrimSpace(line), "\u2014") && !strings.Contains(line, "pay as you go") {
		t.Errorf("null period_end rendered %q, want an em dash or the pay-as-you-go note", line)
	}
	if strings.Contains(stdout, egressEstimateLabel) {
		t.Errorf("the --cost label printed without --cost:\n%s", stdout)
	}
	assertNoSecrets(t, stdout, "sess-live", "tsk_secret")
}

// TestUsageCostHuman: --cost prints the netted estimate with the exact
// disclaimer label; allowance ≥ used prints $0.00.
func TestUsageCostHuman(t *testing.T) {
	testConfigHome(t)
	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}

	fake := &fakeUsageAPI{acct: map[string]any{
		"open_slabs":             float64(1),
		"egress_measurement":     "bytes",
		"egress_used_bytes":      float64(3e12),
		"allowance_egress_bytes": float64(1e12),
		"balance_usd":            "7.50",
		"period_end":             nil,
		"days_left":              float64(10),
	}}
	stubBillingClient(t, fake)

	stdout, _ := captureOutput(t, func() { usageReport([]string{"--cost"}, cfg, usageTestIndex()) })
	for _, want := range []string{"$10.00", egressEstimateLabel} {
		if !strings.Contains(stdout, want) {
			t.Errorf("cost output missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "estimate only") {
		t.Errorf("cost output missing the disclaimer wording:\n%s", stdout)
	}
	assertNoSecrets(t, stdout, "tsk_secret")

	// allowance ≥ used ⇒ $0.00 (the fixture's used is below its allowance).
	coveredAcct := accountObject(t, accountServerJSON)
	coveredAcct["egress_measurement"] = "bytes"
	covered := &fakeUsageAPI{acct: coveredAcct}
	stubBillingClient(t, covered)
	stdout, _ = captureOutput(t, func() { usageReport([]string{"--cost"}, cfg, usageTestIndex()) })
	if !strings.Contains(stdout, "$0.00") {
		t.Errorf("covered egress rendered something other than $0.00:\n%s", stdout)
	}
	if !strings.Contains(stdout, egressEstimateLabel) {
		t.Errorf("covered egress missing the disclaimer label:\n%s", stdout)
	}
}

// TestUsageJSONBillingAdditive: --json keeps every storage key and adds a
// billing object only with a billing credential; --cost adds the estimate
// field.
func TestUsageJSONBillingAdditive(t *testing.T) {
	testConfigHome(t)
	fake := &fakeUsageAPI{acct: map[string]any{
		"network_bytes":          float64(3e12),
		"raw_bytes":              float64(4e12),
		"open_slabs":             float64(3),
		"egress_measurement":     "bytes",
		"egress_used_bytes":      float64(3e12),
		"allowance_egress_bytes": float64(1e12),
		"balance_usd":            "7.50",
		"period_end":             nil,
		"days_left":              float64(29),
	}}
	stubBillingClient(t, fake)
	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}

	stdout, _ := captureOutput(t, func() { usageReport([]string{"--json"}, cfg, usageTestIndex()) })
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("usage --json is not JSON: %v\n%s", err, stdout)
	}
	if got["files"] != float64(2) || got["logical_bytes"] != float64(1536) {
		t.Fatalf("storage keys changed: %#v", got)
	}
	if _, exists := got["stored_bytes_approx"]; exists {
		t.Fatalf("client-side network estimate still present: %#v", got)
	}
	folders, ok := got["folders"].([]any)
	if !ok || len(folders) != 1 || folders[0].(map[string]any)["folder"] != "photos" {
		t.Fatalf("folders = %#v, want the unchanged storage rows", got["folders"])
	}
	billing, ok := got["billing"].(map[string]any)
	if !ok {
		t.Fatalf("billing object missing with a billing credential: %#v", got)
	}
	if billing["network_bytes"] != float64(3e12) || billing["raw_bytes"] != float64(4e12) ||
		billing["open_slabs"] != float64(3) || billing["egress_used_bytes"] != float64(3e12) ||
		billing["allowance_egress_bytes"] != float64(1e12) || billing["balance_usd"] != "7.50" ||
		billing["days_left"] != float64(29) || billing["period_end"] != nil {
		t.Fatalf("billing fields = %#v, want the server values with nulls preserved", billing)
	}
	if _, exists := billing["egress_estimate_usd"]; exists {
		t.Fatalf("estimate field present without --cost: %#v", billing)
	}

	stdout, _ = captureOutput(t, func() { usageReport([]string{"--json", "--cost"}, cfg, usageTestIndex()) })
	var cost map[string]any
	if err := json.Unmarshal([]byte(stdout), &cost); err != nil {
		t.Fatalf("usage --json --cost is not JSON: %v\n%s", err, stdout)
	}
	billingCost, ok := cost["billing"].(map[string]any)
	if !ok || billingCost["egress_estimate_usd"] != "10.00" {
		t.Fatalf("billing --cost = %#v, want egress_estimate_usd 10.00", cost["billing"])
	}
}

// TestUsageNullBillingFields: null open_slabs/egress/balance/days_left/
// period_end render as em dashes in human output and stay null in --json,
// never as Go formatting artifacts.
func TestUsageNullBillingFields(t *testing.T) {
	testConfigHome(t)
	fake := &fakeUsageAPI{acct: map[string]any{
		"open_slabs":             nil,
		"egress_used_bytes":      nil,
		"allowance_egress_bytes": nil,
		"balance_usd":            nil,
		"period_end":             nil,
		"days_left":              nil,
	}}
	stubBillingClient(t, fake)
	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}

	stdout, _ := captureOutput(t, func() { usageReport(nil, cfg, usageTestIndex()) })
	for _, artifact := range []string{"%!", "<nil>", "nil"} {
		if strings.Contains(stdout, artifact) {
			t.Fatalf("null field rendered as %q:\n%s", artifact, stdout)
		}
	}
	for _, prefix := range []string{"logical size", "stored", "network size", "balance", "period ends", "days left"} {
		line := lineWithPrefix(stdout, prefix)
		if line == "" {
			t.Errorf("null field line %q missing:\n%s", prefix, stdout)
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(line), "\u2014") {
			t.Errorf("%s line = %q, want an em dash", prefix, line)
		}
	}

	stdout, _ = captureOutput(t, func() { usageReport([]string{"--json"}, cfg, usageTestIndex()) })
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("usage --json is not JSON: %v", err)
	}
	billing, ok := got["billing"].(map[string]any)
	if !ok {
		t.Fatalf("billing object missing: %#v", got)
	}
	for _, key := range []string{"open_slabs", "egress_used_bytes", "allowance_egress_bytes", "balance_usd", "period_end", "days_left"} {
		v, present := billing[key]
		if !present || v != nil {
			t.Errorf("billing[%q] = %#v, want the server's null preserved", key, v)
		}
	}
}

// TestUsageCostHintWithoutBillingConfig: --cost without billing credentials
// leaves stdout byte-identical and prints an actionable hint on stderr (kept
// off stdout so --json stays parseable); without --cost nothing is said.
func TestUsageCostHintWithoutBillingConfig(t *testing.T) {
	cfg := &Config{IndexerURL: defaultIndexer}

	stdout, stderr := captureOutput(t, func() {
		usageReport([]string{"--cost"}, cfg, usageTestIndex())
	})
	if stdout != usageTestStorageHuman {
		t.Fatalf("--cost changed the storage output without billing config:\n got:\n%q\nwant:\n%q", stdout, usageTestStorageHuman)
	}
	for _, want := range []string{"--cost", "tessera signup", "tessera login"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("hint %q missing %q", stderr, want)
		}
	}

	stdout, stderr = captureOutput(t, func() {
		usageReport([]string{"--json", "--cost"}, cfg, usageTestIndex())
	})
	if stdout != usageTestStorageJSON {
		t.Fatalf("--json --cost changed the storage JSON without billing config:\n got:\n%q\nwant:\n%q", stdout, usageTestStorageJSON)
	}
	if !strings.Contains(stderr, "--cost") {
		t.Errorf("--json --cost hint = %q, want the --cost hint on stderr", stderr)
	}

	_, stderr = captureOutput(t, func() {
		usageReport(nil, cfg, usageTestIndex())
	})
	if stderr != "" {
		t.Fatalf("usage was noisy without --cost: %q", stderr)
	}
}

// TestUsageBillingUnavailable: a failed account fetch degrades to a warning —
// the storage report still prints, --json carries "billing_error" instead of a
// partial billing object, and the session-expiry guidance survives.
func TestUsageBillingUnavailable(t *testing.T) {
	testConfigHome(t)
	fake := &fakeUsageAPI{err: errSessionExpired}
	stubBillingClient(t, fake)
	cfg := &Config{
		IndexerURL:     defaultIndexer,
		BillingAPIURL:  defaultBillingURL,
		BillingSession: "sess-dead",
	}

	stdout, _ := captureOutput(t, func() { usageReport(nil, cfg, usageTestIndex()) })
	if !strings.HasPrefix(stdout, usageTestStorageHuman) {
		t.Fatalf("storage output missing after a billing failure:\n%s", stdout)
	}
	if !strings.Contains(stdout, "unavailable") || !strings.Contains(stdout, "session expired") {
		t.Fatalf("billing failure not surfaced:\n%s", stdout)
	}
	assertNoSecrets(t, stdout, "sess-dead")

	stdout, _ = captureOutput(t, func() { usageReport([]string{"--json"}, cfg, usageTestIndex()) })
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("usage --json is not JSON: %v\n%s", err, stdout)
	}
	if _, ok := got["billing"]; ok {
		t.Fatalf("billing object present after a failed fetch: %#v", got)
	}
	msg, _ := got["billing_error"].(string)
	if !strings.Contains(msg, "session expired") {
		t.Fatalf("billing_error = %q, want the client's guidance", msg)
	}
	if got["files"] != float64(2) {
		t.Fatalf("storage keys changed after a billing failure: %#v", got)
	}
}

// TestUsageBilling401Guidance: with an agent key the server rejects, the human
// billing section and the --json billing_error both carry the shared sign-in
// guidance (signup/login, and `signup --force` for a revoked agent key)
// instead of a bare billing API error.
func TestUsageBilling401Guidance(t *testing.T) {
	testConfigHome(t)
	fake := &fakeUsageAPI{err: parseBillingError(http.StatusUnauthorized, []byte(`{"detail":{"error":"unauthorized"}}`))}
	stubBillingClient(t, fake)
	cfg := &Config{
		IndexerURL:      defaultIndexer,
		BillingAPIURL:   defaultBillingURL,
		BillingAgentKey: "tsk_secret",
	}

	stdout, _ := captureOutput(t, func() { usageReport(nil, cfg, usageTestIndex()) })
	if !strings.Contains(stdout, "unavailable") {
		t.Fatalf("billing failure not surfaced:\n%s", stdout)
	}
	assertSignInGuidance(t, "usage", stdout)
	assertNoSecrets(t, stdout, "tsk_secret")

	stdout, _ = captureOutput(t, func() { usageReport([]string{"--json"}, cfg, usageTestIndex()) })
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("usage --json is not JSON: %v\n%s", err, stdout)
	}
	msg, _ := got["billing_error"].(string)
	if msg == "" {
		t.Fatalf("billing_error missing: %#v", got)
	}
	assertSignInGuidance(t, "usage --json", msg)
	assertNoSecrets(t, stdout, "tsk_secret")
}

// TestUsageFlagsAreBooleans guards the positional() trap: usage's flags take no
// separate value.
func TestUsageFlagsAreBooleans(t *testing.T) {
	for _, flag := range []string{"--cost", "--by-folder", "--json"} {
		if takesValue(flag) {
			t.Errorf("takesValue(%q) = true; it is a boolean and must not consume a value", flag)
		}
	}
}

// TestUsageCostWithoutByteMeter: the client-reported egress counter is
// retired, so a server without egress_measurement=bytes gets no dollar
// estimate from a stale 0, and --json nulls the counter with a note.
func TestUsageCostWithoutByteMeter(t *testing.T) {
	testConfigHome(t)
	fake := &fakeUsageAPI{acct: map[string]any{
		"open_slabs":        float64(1),
		"egress_used_bytes": float64(0),
		"balance_usd":       "0.05",
		"days_left":         float64(4472),
	}}
	stubBillingClient(t, fake)
	cfg := &Config{IndexerURL: defaultIndexer, BillingAPIURL: defaultBillingURL, BillingAgentKey: "tsk_secret"}
	stdout, _ := captureOutput(t, func() { usageReport([]string{"--cost"}, cfg, usageTestIndex()) })
	if strings.Contains(stdout, "$0.00") || strings.Contains(stdout, egressEstimateLabel) {
		t.Fatalf("estimate computed from the retired counter:\n%s", stdout)
	}
	if !strings.Contains(stdout, "about 4472 days (~12 years)") || !strings.Contains(stdout, "estimate") {
		t.Fatalf("days left not labelled as an estimate:\n%s", stdout)
	}
	stdout, _ = captureOutput(t, func() { usageReport([]string{"--json", "--cost"}, cfg, usageTestIndex()) })
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	b := got["billing"].(map[string]any)
	if b["egress_used_bytes"] != nil || b["egress_note"] == nil || b["egress_estimate_usd"] != nil {
		t.Fatalf("billing json = %#v", b)
	}
}
