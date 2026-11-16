package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.sia.tech/core/types"
	"golang.org/x/term"
)

// hash256 is the SDK's 256-bit hash type, aliased so call sites stay short.
type hash256 = types.Hash256

// emitJSON prints v as indented JSON when the caller asked for machine-readable
// output. It is used by every read-only command.
//
// When a newer CLI release is already known, a JSON object also gets an
// additive top-level "update_available": {"current", "latest"} (existing keys
// and their order are untouched; arrays are left as they are). The
// human-readable notice goes to stderr, so stdout stays one JSON document.
func emitJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		fatal("encode json: %v", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, withUpdateAvailable(data), "", "  "); err != nil {
		fatal("encode json: %v", err)
	}
	buf.WriteByte('\n')
	if _, err := os.Stdout.Write(buf.Bytes()); err != nil {
		fatal("write json: %v", err)
	}
}

// hasFlag reports whether name was passed anywhere in args.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// flagValue returns the value of --name value or --name=value. The boolean
// result is false when the flag is absent.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"="), true
		}
	}
	return "", false
}

// positional returns args with all flag pairs removed, so `upload a.txt --json`
// yields ["a.txt"] and `sync watch --interval 30s` yields ["watch"].
func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
			continue
		}
		// A flag written as "--name value" swallows the next argument.
		if !strings.Contains(a, "=") && takesValue(a) && i+1 < len(args) {
			i++
		}
	}
	return out
}

// takesValue lists the long flags that consume the following argument. Any new
// flag with a separate value must be added here, or positional() will treat the
// value as a positional argument. Boolean flags (--yes, --install-service,
// --dry-run, --json) must NOT be listed.
func takesValue(flag string) bool {
	switch flag {
	case "--as", "--root", "--interval", "--policy", "--jobs", "--max-rate", "--name", "--out", "--at", "--profile", "--retain", "--resolve", "--billing-url", "--contact-email", "--email", "--token", "--code", "--pay-currency", "--coin", "--asset", "--adopt", "--limit", "--api-key", "--offset", "--length", "--qr":
		return true
	}
	return false
}

// hashFile returns the hex SHA-256 of a file's contents, streaming so memory
// stays flat regardless of file size.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a crash never leaves a half-written file where a
// complete one used to be.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tessera-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// copyFileAtomic streams src into a temp file next to dst, fsyncs it and then
// renames it over dst. It returns the number of bytes written.
func copyFileAtomic(dst string, r io.Reader, mode os.FileMode) (int64, error) {
	return copyFileAtomicVerify(dst, r, mode, nil)
}

// copyFileAtomicVerify is copyFileAtomic with a check that runs after the temp
// file is fully written and closed but before it replaces dst. When verify
// returns an error the temp file is removed and dst is left untouched.
//
// An existing dst that is not a regular file (a directory, device, FIFO or
// socket) is refused before anything is written, so a download can never
// replace something like /dev/null.
func copyFileAtomicVerify(dst string, r io.Reader, mode os.FileMode, verify func() error) (int64, error) {
	if fi, err := os.Stat(dst); err == nil && !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("refusing to write %s: it exists and is not a regular file", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), tempFilePrefix+"*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	trackTemp(tmpName)
	defer untrackTemp(tmpName)
	defer os.Remove(tmpName)

	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if verify != nil {
		if err := verify(); err != nil {
			return n, err
		}
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return n, err
	}
	return n, os.Rename(tmpName, dst)
}

// durationFlag parses a Go duration flag with a fallback.
func durationFlag(args []string, name string, def time.Duration) time.Duration {
	if v, ok := flagValue(args, name); ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// intFlag parses an integer flag with a fallback.
func intFlag(args []string, name string, def int) int {
	if v, ok := flagValue(args, name); ok {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// truncate shortens s for display in fixed-width columns.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// stdinIsTerminal reports whether it makes sense to ask the user a question.
// A piped stdin (a script, or `tessera folder add | tee log`) must never be
// prompted: the process would block on input that is never coming. It is a var
// so tests can pin the answer; the billing registration gates its optional
// phrase-password prompt on it.
var stdinIsTerminal = func() bool {
	return term.IsTerminal(int(syscall.Stdin))
}

// promptYesNo asks a question and returns the answer. The second result reports
// whether a prompt was actually shown, so callers can fall back to printed
// instructions when running non-interactively.
func promptYesNo(question string, dflt bool) (bool, bool) {
	if !stdinIsTerminal() {
		return false, false
	}
	suffix := " [y/N] "
	if dflt {
		suffix = " [Y/n] "
	}
	fmt.Print(question + suffix)

	// Read a line rather than fmt.Scanln: a bare Enter (meaning "take the
	// default") makes Scanln return an error, which would lose the default.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if err != nil && answer == "" {
		fmt.Println()
		return dflt, true
	}
	switch answer {
	case "":
		return dflt, true
	case "y", "yes":
		return true, true
	default:
		return false, true
	}
}

// formatAge renders a timestamp as a coarse "how long ago" string.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// tempFilePrefix names the partial file a download writes before it is
// verified and renamed into place.
const tempFilePrefix = ".tessera-tmp-"

// staleTempAge is how long a partial download file must have been left
// unchanged before a later download in the same folder removes it. A running
// download writes to its file continuously, so it is never this old.
const staleTempAge = 15 * time.Minute

var (
	tempMu    sync.Mutex
	tempFiles = map[string]bool{}
	tempOnce  sync.Once
)

// trackTemp remembers a partial file so an interrupt (Ctrl-C, SIGTERM)
// removes it before the process exits. The handler is installed on first use.
func trackTemp(name string) {
	tempOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			sig := <-ch
			transferProgress.Finish()
			removeTrackedTemps()
			fmt.Fprintln(os.Stderr, "\nInterrupted; the partial download was removed.")
			code := 130
			if sig == syscall.SIGTERM {
				code = 143
			}
			os.Exit(code)
		}()
	})
	tempMu.Lock()
	tempFiles[name] = true
	tempMu.Unlock()
}

func untrackTemp(name string) {
	tempMu.Lock()
	delete(tempFiles, name)
	tempMu.Unlock()
}

func removeTrackedTemps() {
	tempMu.Lock()
	defer tempMu.Unlock()
	for name := range tempFiles {
		_ = os.Remove(name)
	}
}

// removeStaleTemps deletes partial download files left in dir by an earlier
// download that was killed (for example with SIGKILL, which cannot be caught)
// and returns how many it removed. Only regular files named
// .tessera-tmp-* that have not changed for staleTempAge are touched.
func removeStaleTemps(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tempFilePrefix) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleTempAge {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// checkDownloadDest fails early, before anything is downloaded, when dest
// cannot be written: it is a folder or another non-regular file, or its
// folder (or the nearest existing parent, since missing folders are created)
// is not a writable folder.
func checkDownloadDest(dest string) error {
	if fi, err := os.Stat(dest); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s is a folder; give a file name, for example %s", dest, filepath.Join(dest, "<name>"))
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("refusing to write %s: it exists and is not a regular file", dest)
		}
	}
	dir := filepath.Dir(dest)
	for {
		fi, err := os.Stat(dir)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("cannot save to %s: %s is not a folder", dest, dir)
			}
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot save to %s: %v", dest, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	probe, err := os.CreateTemp(dir, tempFilePrefix+"probe-*")
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		if missing := filepath.Dir(dest); missing != dir {
			return fmt.Errorf("cannot save to %s: the folder %s does not exist and cannot be created in %s (%v); nothing was downloaded", dest, missing, dir, err)
		}
		return fmt.Errorf("cannot save to %s: the folder %s is not writable (%v); nothing was downloaded", dest, dir, err)
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return nil
}
