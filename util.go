package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.sia.tech/core/types"
	"golang.org/x/term"
)

// hash256 is the SDK's 256-bit hash type, aliased so call sites stay short.
type hash256 = types.Hash256

// emitJSON prints v as indented JSON when the caller asked for machine-readable
// output. It is used by every read-only command.
func emitJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatal("encode json: %v", err)
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
	case "--as", "--root", "--interval", "--policy", "--jobs", "--max-rate", "--name", "--out", "--at", "--profile", "--retain", "--resolve":
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
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tessera-tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
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
// prompted: the process would block on input that is never coming.
func stdinIsTerminal() bool {
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
