package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Transfer progress for upload / download / fetch.
//
// Output goes to stderr only, so stdout (and any --json document) stays
// machine-parseable:
//   - stderr is a TTY: a live, redrawn line —
//     "↑ big.iso  uploading shards 41/90  123.4 MiB / 1.0 GiB  12%  8.1 MiB/s  ETA 1m48s"
//   - stderr is not a TTY: a plain line every progressPlainInterval (agents,
//     CI logs), never a carriage-return animation.
//   - --json, --quiet / -q or TESSERA_QUIET=1: nothing at all.
//
// The phase is always shown (checking host funding, waiting for host
// funding, encoding, uploading shards X/N, pinning, downloading) so a long
// step never looks stuck.

type progressMode int

const (
	progressOff progressMode = iota
	progressTTY
	progressPlain
)

var (
	progressTTYInterval   = 200 * time.Millisecond
	progressPlainInterval = 10 * time.Second
	stderrIsTerminal      = func() bool { return term.IsTerminal(int(syscall.Stderr)) }
)

// transferProgress is the active reporter for this command; nil = off. All
// methods are nil-safe so transfer code can call them unconditionally.
var transferProgress *progressReporter

// quietMode is set by --quiet / -q / TESSERA_QUIET: no progress, no
// informational waiting lines (errors still print).
var quietMode bool

// configureProgress picks the mode for a transfer command from its flags and
// whether stderr is a terminal.
func configureProgress(args []string) {
	quietMode = hasFlag(args, "--quiet") || hasFlag(args, "-q") || os.Getenv("TESSERA_QUIET") == "1"
	switch {
	case quietMode || hasFlag(args, "--json"):
		transferProgress = nil
	case stderrIsTerminal():
		transferProgress = newProgressReporter(os.Stderr, progressTTY)
	default:
		transferProgress = newProgressReporter(os.Stderr, progressPlain)
	}
}

type progressReporter struct {
	mu          sync.Mutex
	out         io.Writer
	mode        progressMode
	label       string
	total       int64
	done        int64
	phase       string
	shardsDone  int
	shardsTotal int
	start       time.Time
	running     bool
	stop        chan struct{}
	wg          sync.WaitGroup
	lastWidth   int
	now         func() time.Time
}

func newProgressReporter(out io.Writer, mode progressMode) *progressReporter {
	return &progressReporter{out: out, mode: mode, now: time.Now}
}

// Phase shows a step outside a transfer (e.g. "checking host funding"). On a
// TTY it is drawn as a transient line; in plain mode it prints once.
func (p *progressReporter) Phase(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == phase {
		return
	}
	p.phase = phase
	if p.running {
		if p.mode == progressPlain {
			fmt.Fprintln(p.out, p.lineLocked())
		}
		return
	}
	switch p.mode {
	case progressTTY:
		p.drawLocked(phase + "...")
	case progressPlain:
		fmt.Fprintln(p.out, phase+"...")
	}
}

// Start begins a transfer of total bytes (0 = unknown).
func (p *progressReporter) Start(label string, total int64, phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		p.Finish()
		p.mu.Lock()
	}
	p.label, p.total, p.done, p.phase = label, total, 0, phase
	p.shardsDone, p.shardsTotal = 0, 0
	p.start = p.now()
	p.running = true
	p.stop = make(chan struct{})
	interval := progressTTYInterval
	if p.mode == progressPlain {
		interval = progressPlainInterval
		fmt.Fprintln(p.out, p.lineLocked())
	}
	p.mu.Unlock()

	p.wg.Add(1)
	go func(stop chan struct{}) {
		defer p.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.mu.Lock()
				if p.mode == progressTTY {
					p.drawLocked(p.lineLocked())
				} else {
					fmt.Fprintln(p.out, p.lineLocked())
				}
				p.mu.Unlock()
			}
		}
	}(p.stop)
	if p.mode == progressTTY {
		p.mu.Lock()
		p.drawLocked(p.lineLocked())
		p.mu.Unlock()
	}
}

// Add records n more bytes transferred.
func (p *progressReporter) Add(n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	p.done += n
	if p.total > 0 && p.done > p.total {
		p.done = p.total
	}
	p.mu.Unlock()
}

// SetDone sets the absolute byte count (monotonic).
func (p *progressReporter) SetDone(n int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if n > p.done {
		p.done = n
		if p.total > 0 && p.done > p.total {
			p.done = p.total
		}
	}
	p.mu.Unlock()
}

// Shards updates the shard counter and the "uploading shards X/N" phase.
func (p *progressReporter) Shards(done, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.shardsDone, p.shardsTotal = done, total
	p.phase = fmt.Sprintf("uploading shards %d/%d", done, total)
	p.mu.Unlock()
}

// SetPhase changes the phase of a running transfer.
func (p *progressReporter) SetPhase(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	changed := p.phase != phase
	p.phase = phase
	if changed && p.running {
		if p.mode == progressTTY {
			p.drawLocked(p.lineLocked())
		} else {
			fmt.Fprintln(p.out, p.lineLocked())
		}
	}
	p.mu.Unlock()
}

// Finish stops the redraw loop and clears the live line (TTY), so the
// command's own summary line prints cleanly.
func (p *progressReporter) Finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.running {
		if p.mode == progressTTY && p.lastWidth > 0 {
			p.clearLocked()
		}
		p.mu.Unlock()
		return
	}
	p.running = false
	close(p.stop)
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	if p.mode == progressTTY {
		p.clearLocked()
	}
	p.phase = ""
	p.mu.Unlock()
}

func (p *progressReporter) clearLocked() {
	if p.lastWidth > 0 {
		fmt.Fprint(p.out, "\r"+strings.Repeat(" ", p.lastWidth)+"\r")
		p.lastWidth = 0
	}
}

func (p *progressReporter) drawLocked(line string) {
	if w := terminalWidth(); w > 10 && len([]rune(line)) > w-1 {
		line = string([]rune(line)[:w-2]) + "…"
	}
	pad := ""
	if n := len([]rune(line)); n < p.lastWidth {
		pad = strings.Repeat(" ", p.lastWidth-n)
	}
	fmt.Fprint(p.out, "\r"+line+pad)
	p.lastWidth = len([]rune(line))
}

// lineLocked renders: label  phase  done / total  pct  speed  ETA.
func (p *progressReporter) lineLocked() string {
	parts := []string{}
	if p.label != "" {
		parts = append(parts, p.label)
	}
	if p.phase != "" {
		parts = append(parts, p.phase)
	}
	elapsed := p.now().Sub(p.start)
	if p.total > 0 {
		pct := float64(p.done) * 100 / float64(p.total)
		parts = append(parts, fmt.Sprintf("%s / %s", formatBytes(uint64(p.done)), formatBytes(uint64(p.total))),
			fmt.Sprintf("%.0f%%", pct))
	} else {
		parts = append(parts, formatBytes(uint64(p.done)))
	}
	if elapsed >= time.Second && p.done > 0 {
		speed := float64(p.done) / elapsed.Seconds()
		parts = append(parts, formatBytes(uint64(speed))+"/s")
		if p.total > 0 && p.done < p.total {
			eta := time.Duration(float64(p.total-p.done)/speed) * time.Second
			parts = append(parts, "ETA "+formatETA(eta))
		}
	} else {
		parts = append(parts, fmt.Sprintf("%ds elapsed", int(elapsed.Seconds())))
	}
	return strings.Join(parts, "  ")
}

func formatETA(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

var terminalWidth = func() int {
	w, _, err := term.GetSize(int(syscall.Stderr))
	if err != nil {
		return 0
	}
	return w
}

// countingReader reports every read to fn.
type countingReader struct {
	r  io.Reader
	fn func(int64)
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 && c.fn != nil {
		c.fn(int64(n))
	}
	return n, err
}

// uploadShardTotal is the number of shards an upload of size bytes produces
// (data + parity per 4 MiB-sector slab).
func uploadShardTotal(size int64) int {
	slab := int64(dataShards) * sectorSize
	slabs := (size + slab - 1) / slab
	if slabs < 1 {
		slabs = 1
	}
	return int(slabs) * (dataShards + parityShards)
}

const sectorSize = 1 << 22
