package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestStatusDispatchPassesArgs pins fix 1: the `status` branch of main must
// forward the command arguments to cmdStatus, or `status --json` quietly falls
// back to the human output. statusCommand is the package-level dispatch seam
// (precedent: serviceCommand); replacing it keeps the test network-free. With
// the pre-fix `cmdStatus()` call the stub is never reached and the command
// fatals out instead of returning.
func TestStatusDispatchPassesArgs(t *testing.T) {
	testConfigHome(t)

	orig := statusCommand
	t.Cleanup(func() { statusCommand = orig })
	var got []string
	statusCommand = func(args ...string) { got = append([]string(nil), args...) }

	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })
	os.Args = []string{"tessera", "status", "--json"}
	main()

	if len(got) != 1 || got[0] != "--json" {
		t.Fatalf("status dispatch got args %v, want [--json]", got)
	}
}

// TestAdoptAndLimitValueFlagsRegistered pins fix 2: both flags take a separate
// value, so positional() must skip it. Pre-fix, `sync --adopt local` saw
// "local" as a subcommand and `audit --limit 5` kept "5" as the path prefix.
func TestAdoptAndLimitValueFlagsRegistered(t *testing.T) {
	for _, flag := range []string{"--adopt", "--limit"} {
		if !takesValue(flag) {
			t.Errorf("takesValue(%q) = false; positional() would swallow its value", flag)
		}
	}
	if got := positional([]string{"--adopt", "local"}); len(got) != 0 {
		t.Errorf("positional(sync --adopt local) = %v, want no positionals", got)
	}
	if got := positional([]string{"--limit", "5"}); len(got) != 0 {
		t.Errorf("positional(audit --limit 5) = %v, want no positionals", got)
	}
}

// TestApplyIndexerURLRejectsNonTesseraHost pins fix 3: a value that loadConfig
// would later refuse must be rejected before it can be saved, so the next
// command never bricks on an unloadable config. The seam is a package-level
// var, so this stays exit-free (cmdConfig calls fatal on rejection).
func TestApplyIndexerURLRejectsNonTesseraHost(t *testing.T) {
	home := testConfigHome(t)
	cfg := &Config{IndexerURL: defaultIndexer, AppID: "app-1"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(home, configFileName)
	before, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyIndexerURL(cfg, "http://evil.example.com"); err == nil {
		t.Fatal("applyIndexerURL accepted a non-Tessera host that loadConfig would reject")
	}
	if cfg.IndexerURL != defaultIndexer {
		t.Fatalf("a rejected --indexer value changed the config to %q", cfg.IndexerURL)
	}

	// Nothing on disk changed, so the next command still loads the config.
	if _, err := loadConfig(); err != nil {
		t.Fatalf("config is unloadable after a rejected --indexer: %v", err)
	}
	after, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("config file changed after a rejected --indexer:\nbefore: %s\nafter:  %s", before, after)
	}

	if err := applyIndexerURL(cfg, defaultIndexer); err != nil {
		t.Fatalf("applyIndexerURL(default): %v", err)
	}
	if cfg.IndexerURL != defaultIndexer {
		t.Fatalf("IndexerURL = %q, want %q", cfg.IndexerURL, defaultIndexer)
	}
}
