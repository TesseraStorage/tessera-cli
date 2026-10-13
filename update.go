package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

// updateHint points at the one-command installer; the CLI never installs.
const updateHint = "Update with: curl -fsSL https://tessera.storage/install.sh | sh"

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
	return fmt.Sprintf("A newer tessera is available: %s (you have %s). %s", latest, version, updateHint)
}
