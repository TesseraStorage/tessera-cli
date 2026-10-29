package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// updateRepo is the public release feed the CLI follows. Release tags are the
// version source (Packet 2), so "latest release" is authoritative.
const updateRepo = "TesseraStorage/tessera-cli"

const (
	updateCheckTTL  = 24 * time.Hour
	updateHTTPLimit = 2 * time.Second
)

// updateHint points at 'tessera update' (verified, in-place upgrade).
const updateHint = "Run 'tessera update' to install it."

// fetchLatestTag returns the newest release tag for a repo. It is a package
// var so tests can substitute a stub and never touch the network.
var fetchLatestTag = defaultFetchLatestTag

func defaultFetchLatestTag(repo string) (string, error) {
	client := &http.Client{Timeout: updateHTTPLimit}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tessera-cli/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: HTTP %d", resp.StatusCode)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.TagName), nil
}

// updateCache remembers the last check so calls are throttled to one probe per
// TTL — a quiet background check, never a per-command network call.
type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func updateCachePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update-check.json"), nil
}

func loadUpdateCache() updateCache {
	p, err := updateCachePath()
	if err != nil {
		return updateCache{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return updateCache{}
	}
	var c updateCache
	_ = json.Unmarshal(data, &c)
	return c
}

func saveUpdateCache(c updateCache) {
	p, err := updateCachePath()
	if err != nil {
		return
	}
	data, _ := json.Marshal(c)
	_ = os.WriteFile(p, data, 0o600)
}

// versionParts parses "vX.Y.Z" (leading v and any -prerelease/+build optional)
// into three ints. Unparsable parts are 0.
func versionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		out[i] = n
	}
	return out
}

// compareVersions returns -1, 0 or 1 for a versus b (semver-ish, three parts).
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// checkForUpdate refreshes the throttle cache when it is stale and returns the
// latest known tag. It is best-effort: any network failure keeps the previous
// value and never errors.
func checkForUpdate(cfg *Config) string {
	if version == "dev" || cfg == nil || cfg.UpdateCheckDisabled {
		return ""
	}
	c := loadUpdateCache()
	if time.Since(c.CheckedAt) < updateCheckTTL {
		return c.Latest
	}
	latest, err := fetchLatestTag(updateRepo)
	if err == nil && latest != "" {
		c.Latest = latest
	}
	c.CheckedAt = time.Now()
	saveUpdateCache(c)
	return c.Latest
}

// updateNotice returns a one-line "newer version" message when a newer release
// is known, else "". It never installs anything.
func updateNotice(cfg *Config) string {
	latest := checkForUpdate(cfg)
	if latest == "" || compareVersions(latest, version) <= 0 {
		return ""
	}
	return fmt.Sprintf("New version %s available (you have %s). %s", latest, version, updateHint)
}

// ---------------------------------------------------------------- tessera update

// updateDownloadBase is where release assets live; TESSERA_UPDATE_BASE
// overrides it for tests (".../<tag>/<asset>" layout).
func updateDownloadBase() string {
	if b := os.Getenv("TESSERA_UPDATE_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://github.com/" + updateRepo + "/releases/download"
}

// releaseAssetName is the binary name the release workflow publishes.
func releaseAssetName(goos, goarch string) string {
	name := "tessera-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func httpGetAll(url string, limit int64) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tessera-cli/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// expectedSHA256 finds asset's hash in a SHA256SUMS file.
func expectedSHA256(sums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", asset)
}

// cmdUpdate downloads the latest (or --version) release for this platform,
// verifies it against the release's SHA256SUMS and atomically replaces the
// running binary.
//
//	tessera update [--check] [--version vX.Y.Z] [--force]
func cmdUpdate(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Println(`Usage: tessera update [--check] [--version vX.Y.Z] [--force]

Downloads the newest tessera release for this computer, verifies it against
the release's SHA256SUMS, and replaces this binary in place.
  --check     only report whether an update is available
  --version   install a specific release tag
  --force     reinstall even when already up to date`)
		return
	}
	if err := runUpdate(args); err != nil {
		fatal("update: %v", err)
	}
}

func runUpdate(args []string) error {
	target, _ := flagValue(args, "--version")
	if target == "" {
		latest, err := fetchLatestTag(updateRepo)
		if err != nil {
			return fmt.Errorf("could not check for the latest release: %w", err)
		}
		target = latest
		saveUpdateCache(updateCache{CheckedAt: time.Now(), Latest: latest})
	}
	if !strings.HasPrefix(target, "v") {
		target = "v" + target
	}
	newer := version == "dev" || compareVersions(target, version) > 0
	if hasFlag(args, "--check") {
		if newer {
			fmt.Printf("Update available: %s (you have %s). Run 'tessera update'.\n", target, version)
		} else {
			fmt.Printf("tessera %s is up to date.\n", version)
		}
		return nil
	}
	if !newer && !hasFlag(args, "--force") && !flagPresent(args, "--version") {
		fmt.Printf("tessera %s is up to date.\n", version)
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	asset := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	base := updateDownloadBase() + "/" + target
	fmt.Printf("Downloading tessera %s (%s)...\n", target, asset)
	sums, err := httpGetAll(base+"/SHA256SUMS", 1<<20)
	if err != nil {
		return err
	}
	want, err := expectedSHA256(sums, asset)
	if err != nil {
		return err
	}
	bin, err := httpGetAll(base+"/"+asset, 512<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s); nothing was changed", asset, got, want)
	}
	fmt.Println("Checksum verified (SHA256SUMS).")

	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".tessera-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (%w); re-run with permission to replace it", exe, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// A running .exe cannot be overwritten, but it can be renamed.
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(tmpName, exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}
	} else if err := os.Rename(tmpName, exe); err != nil {
		return err
	}
	fmt.Printf("Updated %s: %s -> %s\n", exe, version, target)
	return nil
}

// flagPresent reports whether name appears at all (with or without a value).
func flagPresent(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func runtimeGOOS() string   { return runtime.GOOS }
func runtimeGOARCH() string { return runtime.GOARCH }
