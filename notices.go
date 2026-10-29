package main

import (
	"fmt"
	"os"
	"time"
)

// Background notices printed around a command:
//   - "New version X available ... Run 'tessera update'": checked at most
//     once a day (cached in update-check.json) in a goroutine, so it never
//     delays the command; printed to stderr after the command finishes.
//   - account readiness ("free trial — first upload is prepaid", "top up to
//     continue", "still getting ready"): printed to stderr before transfer
//     commands when the account is not fully funded, cached for 5 minutes.

// readinessNoticeCommands get the readiness line before they run. Commands
// that print readiness themselves (signup, login, status, account) are not
// listed.
var readinessNoticeCommands = map[string]bool{
	"upload": true, "put": true, "download": true, "get": true,
	"sync": true, "list": true, "ls": true, "share": true, "fetch": true,
	"usage": true, "du": true, "audit": true, "versions": true, "trash": true,
	"delete": true, "rm": true, "find": true, "search": true, "whoami": true,
}

const (
	updateNoticeWait    = 1500 * time.Millisecond
	readinessNoticeWait = 3 * time.Second
)

type backgroundNotices struct {
	update chan string
}

func startBackgroundNotices(cmd string, args []string) *backgroundNotices {
	n := &backgroundNotices{}
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		return n
	}
	cfg, err := loadConfig()
	if err != nil {
		return n
	}
	switch cmd {
	case "version", "--version", "update", "upgrade", "help", "-h", "--help", "uninstall":
	default:
		n.update = make(chan string, 1)
		go func() { n.update <- updateNotice(cfg) }()
	}
	if readinessNoticeCommands[cmd] && os.Getenv("TESSERA_NO_READINESS_NOTICE") == "" {
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

func (n *backgroundNotices) print() {
	if n == nil || n.update == nil {
		return
	}
	select {
	case msg := <-n.update:
		if msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	case <-time.After(updateNoticeWait):
	}
}
