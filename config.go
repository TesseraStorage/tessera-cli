package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	defaultIndexer    = "https://index.tessera.storage"
	defaultBillingURL = "https://tessera.storage/api/v1"
	configDirName     = ".tessera"
	configFileName    = "config.json"
)

// Config stores persistent credentials.
type Config struct {
	IndexerURL      string `json:"indexer_url"`
	AppID           string `json:"app_id"`
	// AppKey is the full Ed25519 private key as 128 hex characters (64 bytes:
	// seed(32) || public_key(32)) -- exactly what the SDK's appKey().export()
	// produces and what connectSDK/connectAPI require. NOT a 32-byte seed: an
	// account's 12-word recovery PHRASE derives a 32-byte seed, but
	// siastorage's deriveAppKey (vendor/go.sia.tech/siastorage/connect.go)
	// expands that seed into the full 64-byte key before it is ever written
	// here. A pre-issued credential handed to you directly
	// (e.g. by an operator or a headless redemption flow) is already in this
	// 128-hex-char shape -- write it here as-is, do not truncate it to 32 bytes.
	AppKey          string `json:"app_key"`
	PhraseEncrypted string `json:"phrase_encrypted"` // AES-GCM encrypted, hex-encoded
	PhraseSalt      string `json:"phrase_salt"`      // argon2 salt, hex-encoded

	// VersionRetention is how many previous copies of a file to keep. 0 means
	// no versions are retained (the old object is deleted on replace).
	VersionRetention int `json:"version_retention,omitempty"`
	// TrashRetentionDays is how long soft-deleted files stay recoverable.
	// 0 means the 30 day default; a negative value disables trash entirely.
	TrashRetentionDays int `json:"trash_retention_days,omitempty"`
	// DefaultJobs limits parallel transfers (0 = the built-in default).
	DefaultJobs int `json:"default_jobs,omitempty"`
	// IndexerCostPerTBMonth is only used by cost forecasts; it lets an
	// operator keep forecasts in step with real pricing.
	IndexerCostPerTBMonth float64 `json:"indexer_cost_per_tb_month,omitempty"`

	// UpdateCheckDisabled turns off the quiet "a newer version is available"
	// check (tessera config --update-check off). The check is throttled,
	// best-effort and only ever prints; it never installs anything.
	UpdateCheckDisabled bool `json:"update_check_disabled,omitempty"`

	// BillingAPIURL is the moneyd API base (…/api/v1). BillingSession holds
	// the tessera_session cookie for human logins; BillingAgentKey holds an
	// agent's tsk_… bearer key. Both secrets live only in this 0600 file.
	BillingAPIURL   string `json:"billing_api_url,omitempty"`
	BillingSession  string `json:"billing_session,omitempty"`
	BillingAgentKey string `json:"billing_agent_key,omitempty"`
}

// configDir returns the directory holding credentials and state. The
// TESSERA_HOME environment variable overrides the default ~/.tessera location,
// which allows multiple accounts (or an isolated test sandbox) on one machine.
func configDir() (string, error) {
	if override := os.Getenv("TESSERA_HOME"); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find home directory: %w", err)
	}
	return filepath.Join(home, configDirName), nil
}

func configFilePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

func loadConfig() (*Config, error) {
	cp, err := configFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(cp)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("corrupt config at %s: %w", cp, err)
	}
	if cfg.IndexerURL == "" {
		cfg.IndexerURL = defaultIndexer
	}
	// Missing billing settings are not an error: configs written before the
	// billing fields existed keep working with the default API base and no
	// stored credentials.
	if cfg.BillingAPIURL == "" {
		cfg.BillingAPIURL = defaultBillingURL
	}
	// Anything not on the Tessera indexer host is rejected rather than silently
	// used: a stale hostname produced an opaque TLS error, and pointing an
	// account at a host it was not registered against is never what the user
	// wants.
	if !isTesseraIndexer(cfg.IndexerURL) {
		return nil, fmt.Errorf("config %s points at %s, but this build only supports %s; run 'tessera login' to reconnect",
			cp, cfg.IndexerURL, defaultIndexer)
	}
	return &cfg, nil
}

// isTesseraIndexer reports whether a URL is a supported indexer host: the
// production indexer, or a loopback host for development and tests. Plain
// http is allowed only on loopback (mirroring isBillingURL), so an app key is
// never sent in clear over a network.
func isTesseraIndexer(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return false
		}
	default:
		return false
	}
	if isLoopbackHost(u.Hostname()) {
		return true
	}
	want, err := url.Parse(defaultIndexer)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, want.Host)
}

// isLoopbackHost reports whether a hostname names the local machine.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// isBillingURL reports whether a URL may be used as the billing API base:
// https everywhere, or plain http only for loopback hosts (127.0.0.1 and
// localhost), so a session or agent key is never sent in clear over a network.
func isBillingURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
	}
	return false
}

// applyBillingURL validates raw and stores it on cfg. It is separate from
// cmdConfig so the validation rule is testable without os.Exit; nothing is
// changed when raw is rejected.
func applyBillingURL(cfg *Config, raw string) error {
	if !isBillingURL(raw) {
		return fmt.Errorf("--billing-url expects an https URL (http is allowed only for 127.0.0.1 or localhost)")
	}
	cfg.BillingAPIURL = strings.TrimRight(raw, "/")
	return nil
}

func saveConfig(cfg *Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create config directory: %w", err)
	}
	cp, err := configFilePath()
	if err != nil {
		return err
	}
	if cfg.IndexerURL == "" {
		cfg.IndexerURL = defaultIndexer
	}
	if cfg.BillingAPIURL == "" {
		cfg.BillingAPIURL = defaultBillingURL
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cp, data, 0600)
}

func deleteConfigFiles() error {
	cp, err := configFilePath()
	if err != nil {
		return err
	}
	os.Remove(cp)
	dir, _ := configDir()
	os.Remove(dir) // only removes if empty
	return nil
}

// deriveKey derives a 32-byte AES key from password + salt using Argon2id.
func deriveKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
}

func encryptPhrase(phrase, password string) (encHex, saltHex string, err error) {
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", "", e
	}
	key := deriveKey(password, salt)
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", "", e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return "", "", e
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return "", "", e
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(phrase), nil)
	return hex.EncodeToString(ciphertext), hex.EncodeToString(salt), nil
}

func decryptPhrase(encHex, saltHex, password string) (string, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", fmt.Errorf("bad salt: %w", err)
	}
	ciphertext, err := hex.DecodeString(encHex)
	if err != nil {
		return "", fmt.Errorf("bad ciphertext: %w", err)
	}
	key := deriveKey(password, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("wrong password or corrupt data")
	}
	return string(plain), nil
}

func formatBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
