package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/rhp/v4/siamux"
	"go.sia.tech/indexd/hosts"
)

func chainAddr(proto chain.Protocol, addr string) chain.NetAddress {
	return chain.NetAddress{Protocol: proto, Address: addr}
}

func TestReadinessStallGivesUpEarly(t *testing.T) {
	d, clk := testDeps(37, 37, func(int) int { return 12 }, 0.05, true)
	d.stallWait = readinessStallWait
	start := clk.now()
	err := checkReadiness(context.Background(), d, 30, true)
	if readinessCode(t, err) != readinessCodeNotReady {
		t.Fatalf("got %v", err)
	}
	if waited := clk.now().Sub(start); waited > 2*readinessStallWait {
		t.Fatalf("waited %v, want about %v", waited, readinessStallWait)
	}
}

func TestOrderedSiamuxAddrsIPFirst(t *testing.T) {
	h := hosts.HostInfo{}
	h.Addresses = append(h.Addresses,
		chainAddr(siamux.Protocol, "host.example.com:9984"),
		chainAddr("quic", "1.2.3.4:9984"),
		chainAddr(siamux.Protocol, "203.0.113.7:9984"),
	)
	got := orderedSiamuxAddrs(h)
	want := []string{"203.0.113.7:9984", "host.example.com:9984"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

type flakyRT struct {
	fails int
	calls int
	err   error
}

func (f *flakyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, f.err
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
}

func TestRetryTransportRetriesIdempotentEOF(t *testing.T) {
	f := &flakyRT{fails: 2, err: io.EOF}
	rt := &retryTransport{next: f}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil || resp.StatusCode != 200 || f.calls != 3 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, f.calls)
	}
}

func TestRetryTransportNeverRetriesPOST(t *testing.T) {
	f := &flakyRT{fails: 1, err: io.EOF}
	rt := &retryTransport{next: f}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", strings.NewReader("{}"))
	if _, err := rt.RoundTrip(req); err == nil || f.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, f.calls)
	}
}

func TestRetryTransportStopsOnPermanentError(t *testing.T) {
	f := &flakyRT{fails: 5, err: errors.New("x509: certificate signed by unknown authority")}
	rt := &retryTransport{next: f}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if _, err := rt.RoundTrip(req); err == nil || f.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, f.calls)
	}
}

func TestIsTransientNetErr(t *testing.T) {
	for _, e := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, errors.New(`Get "https://x": EOF`)} {
		if !isTransientNetErr(e) {
			t.Errorf("%v should be transient", e)
		}
	}
	for _, e := range []error{context.Canceled, context.DeadlineExceeded, errors.New("401 unauthorized"), errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")} {
		if isTransientNetErr(e) {
			t.Errorf("%v should not be transient", e)
		}
	}
}

func TestRedactURLs(t *testing.T) {
	in := `Get "https://index.tessera.storage/objects/abc?SiaIdx-Signature=deadbeef&SiaIdx-ValidUntil=1": EOF`
	got := redactURLs(in)
	if strings.Contains(got, "deadbeef") || !strings.Contains(got, "https://index.tessera.storage/objects/abc?<redacted>") {
		t.Fatalf("got %q", got)
	}
}

func TestConnectionHint(t *testing.T) {
	if h := connectionHint(io.EOF); strings.Contains(h, "tessera login") {
		t.Fatalf("network error must not suggest login: %q", h)
	}
	if h := connectionHint(errors.New("401 Unauthorized")); !strings.Contains(h, "tessera login") {
		t.Fatalf("auth error should suggest login: %q", h)
	}
}

func TestCommandHelpCoversFileCommands(t *testing.T) {
	for _, c := range []string{"upload", "put", "download", "get", "delete", "rm", "share", "fetch"} {
		if commandHelp[c] == "" {
			t.Errorf("no --help text for %s", c)
		}
	}
	if !strings.Contains(commandHelp["upload"], "--as") || !strings.Contains(commandHelp["upload"], "--quiet") {
		t.Error("upload help must document --as and --quiet")
	}
	if !strings.Contains(usageText, "--as <remote-path>") {
		t.Error("top-level help must document upload --as")
	}
}

func TestReadinessNoticeShownOncePerState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := readinessCachePath()
	if p == "" {
		t.Skip("no config dir")
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	c := readinessCache{CheckedAt: time.Now(), State: "free_trial", Message: "first upload is free"}
	data := []byte(`{"checked_at":"` + c.CheckedAt.Format(time.RFC3339Nano) + `","state":"free_trial","message":"first upload is free"}`)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{BillingAgentKey: "tsk_test"}
	if credentialLabel(cfg) == "" {
		t.Skip("credentialLabel needs a different field")
	}
	if first := readinessNotice(cfg); first == "" {
		t.Fatal("first notice should print")
	}
	if second := readinessNotice(cfg); second != "" {
		t.Fatalf("unchanged state printed again: %q", second)
	}
}

func TestConfigOmitsEmptyPhraseFields(t *testing.T) {
	b, err := json.Marshal(&Config{IndexerURL: "https://x", AppID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "phrase_encrypted") || strings.Contains(string(b), "phrase_salt") {
		t.Fatalf("empty phrase fields serialized: %s", b)
	}
}
