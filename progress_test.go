package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestProgressLineHasBytesPercentSpeedETA(t *testing.T) {
	clock := time.Unix(1000, 0)
	p := newProgressReporter(&syncBuf{}, progressPlain)
	p.now = func() time.Time { return clock }
	p.Start("↑ f.bin", 100<<20, "encoding")
	defer p.Finish()
	clock = clock.Add(10 * time.Second)
	p.Shards(9, 30)
	p.SetDone(30 << 20)
	line := p.lineLocked()
	for _, want := range []string{"↑ f.bin", "uploading shards 9/30", "/ 100.00 MiB", "30%", "/s", "ETA 23s"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
}

func TestProgressTTYRedrawsAndClears(t *testing.T) {
	old := progressTTYInterval
	progressTTYInterval = 5 * time.Millisecond
	defer func() { progressTTYInterval = old }()
	buf := &syncBuf{}
	p := newProgressReporter(buf, progressTTY)
	p.Phase("checking host funding")
	p.Start("↓ x", 1000, "downloading")
	p.Add(500)
	time.Sleep(30 * time.Millisecond)
	p.Finish()
	out := buf.String()
	if !strings.Contains(out, "checking host funding...") || !strings.Contains(out, "50%") {
		t.Fatalf("tty output: %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("tty mode must redraw in place, not print lines: %q", out)
	}
	if !strings.HasSuffix(out, "\r") {
		t.Fatalf("line not cleared at finish: %q", out)
	}
}

func TestProgressPlainPrintsLinesNoCarriageReturn(t *testing.T) {
	old := progressPlainInterval
	progressPlainInterval = 5 * time.Millisecond
	defer func() { progressPlainInterval = old }()
	buf := &syncBuf{}
	p := newProgressReporter(buf, progressPlain)
	p.Start("↑ y", 10, "encoding")
	time.Sleep(25 * time.Millisecond)
	p.SetPhase("pinning")
	p.Finish()
	out := buf.String()
	if strings.Contains(out, "\r") || strings.Count(out, "\n") < 2 || !strings.Contains(out, "pinning") {
		t.Fatalf("plain output: %q", out)
	}
}

func TestConfigureProgressModes(t *testing.T) {
	origTTY := stderrIsTerminal
	defer func() { stderrIsTerminal = origTTY; transferProgress = nil; quietMode = false }()
	t.Setenv("TESSERA_QUIET", "")
	stderrIsTerminal = func() bool { return true }
	configureProgress(nil)
	if transferProgress == nil || transferProgress.mode != progressTTY {
		t.Fatal("tty: want live bar")
	}
	stderrIsTerminal = func() bool { return false }
	configureProgress(nil)
	if transferProgress == nil || transferProgress.mode != progressPlain {
		t.Fatal("non-tty: want plain lines")
	}
	for _, args := range [][]string{{"--json"}, {"--quiet"}, {"-q"}} {
		configureProgress(args)
		if transferProgress != nil {
			t.Fatalf("%v: want no progress", args)
		}
	}
	t.Setenv("TESSERA_QUIET", "1")
	configureProgress(nil)
	if transferProgress != nil || !quietMode {
		t.Fatal("TESSERA_QUIET=1: want no progress")
	}
	// nil reporter is safe to use.
	var nilp *progressReporter
	nilp.Start("x", 1, "y")
	nilp.Add(1)
	nilp.Shards(1, 2)
	nilp.SetPhase("z")
	nilp.Phase("z")
	nilp.Finish()
}

func TestUploadShardTotal(t *testing.T) {
	if got := uploadShardTotal(1); got != 30 {
		t.Fatalf("1 byte: %d", got)
	}
	if got := uploadShardTotal(40<<20 + 1); got != 60 {
		t.Fatalf("40MiB+1: %d", got)
	}
}
