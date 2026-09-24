package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

const (
	defaultIndexer = "https://index.tessera.storage"
	configDirName  = ".tessera"
	configFileName = "config.json"
)

// Config stores persistent credentials.
type Config struct {
	IndexerURL      string `json:"indexer_url"`
	AppID           string `json:"app_id"`
	AppKey          string `json:"app_key"`          // hex-encoded 32-byte seed
	PhraseEncrypted string `json:"phrase_encrypted"` // AES-GCM encrypted, hex-encoded
	PhraseSalt      string `json:"phrase_salt"`      // argon2 salt, hex-encoded
}

func configDir() (string, error) {
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
	return &cfg, nil
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
