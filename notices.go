package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Background notices printed around a command:
//   - "New version X available ... Run 'tessera update'": printed to stderr
//     after EVERY command (including failed commands, usage errors and
//     --help) whenever a newer release is known. The known latest version
//     comes from the update-check.json cache, from the X-Tessera-Latest-CLI
//     header the service sends with its API responses, and from a refresh
//     started in the background when the cache is older than updateCheckTTL.
//     The command never waits for that refresh; only at exit, and only when a
//     refresh is in flight, the CLI waits up to updateNoticeWait for it and
//     otherwise uses the cached value. Off with 'tessera config
//     --update-check off'. Never installs anything.
//   - account readiness ("top up to continue", "still getting ready"): printed
//     to stderr before transfer commands only when an upload cannot run, at
//     most once per state every 6 hours, cached for 5 minutes. A working
//     technical validation is never repeated here (status/account/usage
//     explain it).

// readinessNoticeCommands get the readiness line before they run. Commands
// that print readiness themselves (signup, login, status, account, usage)
// and read-only listings are not listed.
var readinessNoticeCommands = map[string]bool{
	"upload": true, "put": true, "download": true, "get": true,
	"sync": true, "share": true, "fetch": true,
}

const (
	updateNoticeWait    = 1500 * time.Millisecond
	readinessNoticeWait = 3 * time.Second
)

// updateNoticeSkip are commands that handle versions themselves.
var updateNoticeSkip = map[string]bool{
	"version": true, "--version": true, "update": true, "upgrade": true, "uninstall": true,
}

type backgroundNotices struct {
	enabled bool
	refresh chan string // result of a background refresh, when one runs
	once    sync.Once
}

// activeNotices lets exit paths (fatal, usage errors) print the update
// notice before the process ends.
var activeNotices *backgroundNotices

func startBackgroundNotices(cmd string, args []string) *backgroundNotices {
	n := &backgroundNotices{}
	activeNotices = n
	if !updateNoticeSkip[cmd] && updateChecksEnabled(updateCheckConfig()) {
		n.enabled = true
		if time.Since(loadUpdateCache().CheckedAt) >= updateCheckTTL {
			n.refresh = make(chan string, 1)
			go func() { n.refresh <- refreshLatestTag() }()
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return n
	}
	help := hasFlag(args, "--help") || hasFlag(args, "-h")
	if !help && readinessNoticeCommands[cmd] && os.Getenv("TESSERA_NO_READINESS_NOTICE") == "" {
		ch := make(chan string, 1)
		go func() { ch <- readinessNotice(cfg) }()
		select {
		case msg := <-ch:
			if msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
		case <-time.After(readinessNoticeWait):
		}
	}
	return n
}

// print writes the update notice (at most once per process) to stderr.
func (n *backgroundNotices) print() {
	if n == nil {
		return
	}
	n.once.Do(func() {
		if !n.enabled {
			return
		}
		refreshed := ""
		if n.refresh != nil {
			select {
			case refreshed = <-n.refresh:
			case <-time.After(updateNoticeWait):
			}
		}
		latest := newestTag(refreshed, rememberServerLatest(), loadUpdateCache().Latest)
		if msg := noticeForLatest(latest); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	})
}

// flushNotices prints pending notices; called on every exit path.
func flushNotices() { activeNotices.print() }

// exitWithNotices prints pending notices, then exits with code.
func exitWithNotices(code int) {
	flushNotices()
	os.Exit(code)
}
