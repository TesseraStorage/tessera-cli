package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cmdConfig shows or changes persistent settings.
//
//	tessera config
//	tessera config --version-retention 5
//	tessera config --trash-retention 14
//	tessera config --default-jobs 4
func cmdConfig(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	changed := false
	if v, ok := flagValue(args, "--version-retention"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fatal("--version-retention expects a non-negative integer")
		}
		cfg.VersionRetention = n
		changed = true
	}
	if v, ok := flagValue(args, "--trash-retention"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			fatal("--trash-retention expects a number of days (0 = default 30, negative disables)")
		}
		cfg.TrashRetentionDays = n
		changed = true
	}
	if v, ok := flagValue(args, "--default-jobs"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fatal("--default-jobs expects a positive integer")
		}
		cfg.DefaultJobs = n
		changed = true
	}
	if v, ok := flagValue(args, "--cost-per-tb-month"); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			fatal("--cost-per-tb-month expects a non-negative number")
		}
		cfg.IndexerCostPerTBMonth = f
		changed = true
	}
	if v, ok := flagValue(args, "--indexer"); ok {
		if !strings.HasPrefix(v, "http") {
			fatal("--indexer expects an http(s) URL")
		}
		cfg.IndexerURL = v
		changed = true
	}

	if changed {
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		fmt.Println("Settings saved.")
		fmt.Println()
	}

	roots, _ := listSyncRoots()
	trash, _ := loadTrash()
	p, _ := configFilePath()

	if hasFlag(args, "--json") {
		emitJSON(map[string]interface{}{
			"config_path":          p,
			"indexer":              cfg.IndexerURL,
			"app_id":               cfg.AppID,
			"version_retention":    cfg.VersionRetention,
			"trash_retention_days": cfg.TrashRetentionDays,
			"default_jobs":         cfg.DefaultJobs,
			"sync_folders":         len(roots),
			"trashed_items":        len(trash.Entries),
		})
		return
	}

	fmt.Printf("Config file:      %s\n", p)
	fmt.Printf("Indexer:          %s\n", cfg.IndexerURL)
	fmt.Printf("App ID:           %s\n", cfg.AppID)
	fmt.Printf("Sync folders:     %d\n", len(roots))
	fmt.Printf("Trashed items:    %d\n", len(trash.Entries))
	fmt.Println()
	fmt.Printf("Version retention:  %s\n", describeVersions(cfg.VersionRetention))
	fmt.Printf("Trash retention:    %s\n", describeTrash(cfg.TrashRetentionDays))
	fmt.Printf("Default jobs:       %s\n", describeJobs(cfg.DefaultJobs))
	fmt.Println()
	fmt.Println("Change with: tessera config --version-retention 5")
	fmt.Println("             tessera config --trash-retention 14")
	fmt.Println("             tessera config --default-jobs 4")
	fmt.Println()
	fmt.Println("Per-folder options live in 'tessera sync list' and .tesseraignore.")
}

func describeVersions(n int) string {
	switch {
	case n < 0:
		return "off (previous copies are deleted on replace)"
	case n == 0:
		return fmt.Sprintf("%d previous copy per file (default)", DefaultVersionRetention)
	default:
		return fmt.Sprintf("%d previous copy(ies) per file", n)
	}
}

func describeTrash(n int) string {
	switch {
	case n < 0:
		return "off (delete is immediate)"
	case n == 0:
		return "30 days (default)"
	default:
		return fmt.Sprintf("%d days", n)
	}
}

func describeJobs(n int) string {
	if n <= 0 {
		return "automatic"
	}
	return fmt.Sprintf("%d parallel transfer(s)", n)
}

// serviceInstallPath returns where a service definition is written.
func serviceInstallPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tessera", "service"), nil
}

// cmdService manages running the watcher as a background service. Rather than
// writing platform service definitions behind the user's back, it prints the
// exact commands and, where a template is safe, installs it.
func cmdService(args []string) {
	pos := positional(args)
	sub := "status"
	if len(pos) > 0 {
		sub = pos[0]
	}

	switch sub {
	case "install":
		serviceInstall(args)
	case "status":
		serviceStatus()
	case "print", "template":
		servicePrint()
	case "start", "stop", "uninstall", "restart":
		fmt.Printf("This build prints service definitions rather than modifying your\n")
		fmt.Printf("system automatically. Run 'tessera service print' and follow the\n")
		fmt.Printf("instructions for your platform.\n")
		os.Exit(1)
	default:
		usageError("service install|status|print")
	}
}

func servicePrint() {
	exe, err := os.Executable()
	if err != nil {
		exe = "tessera"
	}
	fmt.Println("Platform service definition for the Tessera watcher")
	fmt.Println()
	fmt.Println("── macOS (launchd) ─────────────────────────────────────────")
	fmt.Println("Save as ~/Library/LaunchAgents/dev.siagate.tessera.plist, then:")
	fmt.Println("  launchctl load ~/Library/LaunchAgents/dev.siagate.tessera.plist")
	fmt.Println()
	fmt.Printf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.siagate.tessera</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>sync</string><string>watch</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s/.tessera/watch.log</string>
  <key>StandardErrorPath</key><string>%s/.tessera/watch.err</string>
</dict></plist>
`, exe, homeDir(), homeDir())

	fmt.Println()
	fmt.Println("── Linux (systemd user unit) ───────────────────────────────")
	fmt.Println("Save as ~/.config/systemd/user/tessera.service, then:")
	fmt.Println("  systemctl --user enable --now tessera")
	fmt.Println()
	fmt.Printf(`[Unit]
Description=Tessera folder sync

[Service]
ExecStart=%s sync watch
Restart=always

[Install]
WantedBy=default.target
`, exe)

	fmt.Println()
	fmt.Println("── Windows (Task Scheduler) ────────────────────────────────")
	fmt.Println("  schtasks /create /tn Tessera /sc onlogon /rl limited ^")
	fmt.Printf("    /tr \"\\\"%s\\\" sync watch\"\n", exe)
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return h
}

// serviceInstall writes the definition to ~/.tessera/service/ for the user to
// move into place, which avoids silently mutating system state.
func serviceInstall(args []string) {
	dir, err := serviceInstallPath()
	if err != nil {
		fatal("%v", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		fatal("%v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "tessera"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.siagate.tessera</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>sync</string><string>watch</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
`, exe)
	if err := os.WriteFile(filepath.Join(dir, "dev.siagate.tessera.plist"), []byte(plist), 0644); err != nil {
		fatal("%v", err)
	}

	unit := fmt.Sprintf(`[Unit]
Description=Tessera folder sync

[Service]
ExecStart=%s sync watch
Restart=always

[Install]
WantedBy=default.target
`, exe)
	if err := os.WriteFile(filepath.Join(dir, "tessera.service"), []byte(unit), 0644); err != nil {
		fatal("%v", err)
	}

	fmt.Printf("Service definitions written to %s\n\n", dir)
	fmt.Println("macOS:  cp dev.siagate.tessera.plist ~/Library/LaunchAgents/ && \\")
	fmt.Println("        launchctl load ~/Library/LaunchAgents/dev.siagate.tessera.plist")
	fmt.Println("Linux:  cp tessera.service ~/.config/systemd/user/ && \\")
	fmt.Println("        systemctl --user enable --now tessera")
	fmt.Println()
	fmt.Println("Nothing was installed into your system automatically — review the")
	fmt.Println("files first, then move them into place.")
}

func serviceStatus() {
	dir, err := serviceInstallPath()
	if err != nil {
		fatal("%v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		fmt.Println("No background service definitions installed.")
		fmt.Println("Run 'tessera service install' to generate them.")
		return
	}
	fmt.Printf("Service definitions in %s:\n", dir)
	for _, e := range ents {
		fmt.Printf("  %s\n", e.Name())
	}
	fmt.Println()
	fmt.Println("Check whether the watcher is actually running with 'tessera sync status'.")
}
