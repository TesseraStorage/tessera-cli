package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Where the CLI learns the newest release, in order of preference:
//  1. the X-Tessera-Latest-CLI header on any response from the Tessera API
//     (free: normal commands already talk to it);
//  2. GET <api>/cli/latest on the Tessera API;
//  3. the GitHub releases API (rate-limited per IP; last resort).

const latestCLIHeader = "X-Tessera-Latest-CLI"

var releaseTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// userAgent identifies this build to the Tessera API, e.g.
// "tessera-cli/v1.17.0 (linux/amd64)". The service never requires it; it
// only shows which versions are still in use.
func userAgent() string {
	return fmt.Sprintf("tessera-cli/%s (%s/%s)", version, runtimeGOOS(), runtimeGOARCH())
}

// apiBaseURL is the Tessera API base the billing client uses.
func apiBaseURL() string {
	if override := os.Getenv("TESSERA_BILLING_URL"); override != "" {
		return strings.TrimRight(override, "/")
	}
	if cfg, err := loadConfig(); err == nil && cfg.BillingAPIURL != "" {
		return strings.TrimRight(cfg.BillingAPIURL, "/")
	}
	return defaultBillingURL
}

var (
	apiHostOnce sync.Once
	apiHost     string
	serverTag   atomic.Value // string
)

func trustedLatestHost(host string) bool {
	apiHostOnce.Do(func() {
		if u, err := url.Parse(apiBaseURL()); err == nil {
			apiHost = u.Host
		}
	})
	return host != "" && strings.EqualFold(host, apiHost)
}

// noteLatestHeader records the latest release the Tessera API advertises.
func noteLatestHeader(req *http.Request, resp *http.Response) {
	if resp == nil || req == nil || req.URL == nil {
		return
	}
	tag := strings.TrimSpace(resp.Header.Get(latestCLIHeader))
	if tag == "" || !releaseTagRE.MatchString(tag) || !trustedLatestHost(req.URL.Host) {
		return
	}
	serverTag.Store(tag)
}

func serverLatestTag() string {
	s, _ := serverTag.Load().(string)
	return s
}

// rememberServerLatest saves a header-learned version into the cache (so the
// next command knows it without any network) and returns it.
func rememberServerLatest() string {
	s := serverLatestTag()
	if s == "" {
		return ""
	}
	c := loadUpdateCache()
	if newest := newestTag(c.Latest, s); newest != c.Latest || time.Since(c.CheckedAt) >= updateCheckTTL {
		c.Latest = newest
		c.CheckedAt = time.Now()
		saveUpdateCache(c)
	}
	return s
}

// newestTag returns the highest release tag among tags ("" when none).
func newestTag(tags ...string) string {
	best := ""
	for _, t := range tags {
		if t == "" {
			continue
		}
		if best == "" || compareVersions(t, best) > 0 {
			best = t
		}
	}
	return best
}

// updateCheckConfig is the config for the update check: a machine that is
// not logged in yet (no config file) still gets the notice.
func updateCheckConfig() *Config {
	cfg, err := loadConfig()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}
		}
		return nil
	}
	return cfg
}

func updateChecksEnabled(cfg *Config) bool {
	return version != "dev" && cfg != nil && !cfg.UpdateCheckDisabled
}

// noticeForLatest is the one-line notice for a known latest tag, or "".
func noticeForLatest(latest string) string {
	if version == "dev" || latest == "" || compareVersions(latest, version) <= 0 {
		return ""
	}
	return fmt.Sprintf("New version %s available (you have %s). %s", latest, version, updateHint)
}

// refreshLatestTag asks for the newest release and updates the cache. Any
// failure keeps the cached value.
func refreshLatestTag() string {
	latest, err := fetchLatestTag(updateRepo)
	c := loadUpdateCache()
	if err == nil && latest != "" {
		c.Latest = newestTag(latest, serverLatestTag())
	}
	c.CheckedAt = time.Now()
	saveUpdateCache(c)
	return c.Latest
}

// knownUpdate is the additive "update_available" object for --json output:
// set only when a newer release is already known (cache or API header). It
// never makes a network call.
func knownUpdate() map[string]string {
	if !updateChecksEnabled(updateCheckConfig()) {
		return nil
	}
	latest := newestTag(serverLatestTag(), loadUpdateCache().Latest)
	if latest == "" || compareVersions(latest, version) <= 0 {
		return nil
	}
	return map[string]string{"current": version, "latest": latest}
}

// withUpdateAvailable appends "update_available" to a JSON object document
// when an update is known, without touching any existing key or its order.
// Arrays, other values and documents that already carry the key are
// returned unchanged.
func withUpdateAvailable(doc []byte) []byte {
	trimmed := strings.TrimSpace(string(doc))
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return doc
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return doc
	}
	if _, exists := probe["update_available"]; exists {
		return doc
	}
	info := knownUpdate()
	if info == nil {
		return doc
	}
	extra, err := json.Marshal(info)
	if err != nil {
		return doc
	}
	body := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	sep := ","
	if body == "" {
		sep = ""
	}
	return []byte("{" + body + sep + `"update_available":` + string(extra) + "}")
}

// fetchLatestFromAPI reads GET <api>/cli/latest.
func fetchLatestFromAPI() (string, error) {
	client := &http.Client{Timeout: updateHTTPLimit}
	req, err := http.NewRequest(http.MethodGet, apiBaseURL()+"/cli/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	tag := strings.TrimSpace(out.Version)
	if !releaseTagRE.MatchString(tag) {
		return "", fmt.Errorf("latest release: unexpected version %q", tag)
	}
	return tag, nil
}
