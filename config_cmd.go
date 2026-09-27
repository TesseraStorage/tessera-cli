package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Service identity. The launchd label is reverse-DNS because that is what
// launchd expects; it names the vendor, not a domain we operate.
const (
	serviceLabel   = "io.tessera.watcher"
	servicePlist   = serviceLabel + ".plist"
	serviceUnit    = "tessera-watch.service"
	serviceTask    = "Tessera"
	serviceUnitAlt = "tessera"
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

// ---------------------------------------------------------------------------
// Background watcher service
// ---------------------------------------------------------------------------

// serviceDefinitionDir is the scratch directory holding a copy of the
// generated definition, useful for review before installing.
func serviceDefinitionDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "service"), nil
}

// watcherExecutable returns the absolute path to this binary, which the service
// definition must reference. A relative path would break under launchd/systemd.
func watcherExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine the path to this binary: %w", err)
	}
	return filepath.Abs(exe)
}

// launchAgentPath is where macOS expects a per-user launch agent.
func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", servicePlist), nil
}

// systemdUserUnitPath is where Linux expects a per-user systemd unit.
func systemdUserUnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", serviceUnit), nil
}

// plistContent renders the launchd agent. It is a pure function so the exact
// installed definition can be asserted in tests.
func plistContent(exe string) string {
	home, _ := os.UserHomeDir()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>sync</string><string>watch</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, serviceLabel, exe,
		filepath.Join(home, ".tessera", "watch.log"),
		filepath.Join(home, ".tessera", "watch.err"))
}

// unitContent renders the systemd user unit.
func unitContent(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=Tessera folder sync

[Service]
ExecStart=%s sync watch
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exe)
}

// servicePlan is the resolved set of actions for this platform: where the
// definition is installed and which command activates it.
type servicePlan struct {
	Platform string
	DefPath  string
	Content  string
	// InstallCmd / RemoveCmd are run with the definition already in place.
	InstallCmd []string
	RemoveCmd  []string
	// BootstrapCmd loads the definition. On launchd a freshly written agent is
	// not known to launchd at all, so it must be bootstrapped before it can be
	// started; systemd needs no equivalent step.
	BootstrapCmd []string
	// BootstrapOptional marks BootstrapCmd as best-effort, which it is when
	// unloading an existing agent is the expected failure.
	BootstrapOptional bool
	// Hint is printed when the platform is supported but activation is manual.
	Hint string
}

// planService resolves the platform-specific install plan. It returns an error
// on platforms we cannot configure automatically.
func planService(exe string) (servicePlan, error) {
	switch runtime.GOOS {
	case "darwin":
		def, err := launchAgentPath()
		if err != nil {
			return servicePlan{}, err
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		return servicePlan{
			Platform: "launchd (macOS)",
			DefPath:  def,
			Content:  plistContent(exe),
			// bootout may fail if nothing is loaded yet; that is not an error.
			BootstrapCmd:      []string{"launchctl", "bootout", domain + "/" + serviceLabel},
			BootstrapOptional: true,
			InstallCmd:        []string{"launchctl", "bootstrap", domain, def},
			RemoveCmd:         []string{"launchctl", "bootout", domain + "/" + serviceLabel},
			Hint: "If launchctl rejects the agent, load it by hand and check the output:\n" +
				"      launchctl bootstrap " + domain + " " + def,
		}, nil
	case "linux":
		def, err := systemdUserUnitPath()
		if err != nil {
			return servicePlan{}, err
		}
		return servicePlan{
			Platform:   "systemd user unit (Linux)",
			DefPath:    def,
			Content:    unitContent(exe),
			InstallCmd: []string{"systemctl", "--user", "enable", "--now", serviceUnit},
			RemoveCmd:  []string{"systemctl", "--user", "disable", "--now", serviceUnit},
			Hint: "systemctl --user needs a running user session. If it fails with\n" +
				"  'Failed to connect to bus', enable lingering once:\n" +
				"      loginctl enable-linger $USER\n" +
				"  then re-run: tessera service install --yes",
		}, nil
	case "windows":
		return servicePlan{
			Platform: "Scheduled Task (Windows)",
			DefPath:  "Task Scheduler task \"" + serviceTask + "\"",
			Content:  "",
			InstallCmd: []string{"schtasks", "/create", "/f", "/tn", serviceTask, "/sc", "onlogon",
				"/rl", "limited", "/tr", `"` + exe + `" sync watch`},
			RemoveCmd: []string{"schtasks", "/delete", "/f", "/tn", serviceTask},
		}, nil
	default:
		return servicePlan{}, fmt.Errorf("automatic service installation is not supported on %s", runtime.GOOS)
	}
}

// serviceCommand is a test seam: tests replace it to capture the commands that
// would run instead of mutating the host.
var serviceCommand = func(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// serviceRun prints and then executes a command, returning its output.
func serviceRun(name string, args ...string) error {
	fmt.Printf("  $ %s\n", strings.Join(append([]string{name}, args...), " "))
	out, err := serviceCommand(name, args...)
	if len(bytes.TrimSpace(out)) > 0 {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Printf("      %s\n", line)
		}
	}
	if err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

// installService writes the definition and activates it.
func installService(exe string, plan servicePlan, activate bool) error {
	if plan.Content != "" {
		if err := os.MkdirAll(filepath.Dir(plan.DefPath), 0755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(plan.DefPath), err)
		}
		if err := os.WriteFile(plan.DefPath, []byte(plan.Content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", plan.DefPath, err)
		}
		fmt.Printf("Definition: %s\n", plan.DefPath)
	} else {
		fmt.Printf("Definition: %s\n", plan.DefPath)
	}

	if !activate {
		return nil
	}

	fmt.Println("Activating:")
	if len(plan.BootstrapCmd) > 0 {
		if plan.BootstrapOptional {
			// Replacing an existing agent: an unload failure just means it was
			// not loaded, which is fine.
			_, _ = serviceCommand(plan.BootstrapCmd[0], plan.BootstrapCmd[1:]...)
		} else if err := serviceRun(plan.BootstrapCmd[0], plan.BootstrapCmd[1:]...); err != nil {
			return err
		}
	}
	if err := serviceRun(plan.InstallCmd[0], plan.InstallCmd[1:]...); err != nil {
		if plan.Hint != "" {
			return fmt.Errorf("%w\n\n%s", err, plan.Hint)
		}
		return err
	}
	return nil
}

// cmdService manages the background watcher.
//
//	tessera service install [--yes]     install and activate
//	tessera service print               show definitions without installing
//	tessera service status              show what is installed
//	tessera service start|stop|restart|uninstall [--yes]
func cmdService(args []string) {
	pos := positional(args)
	sub := "status"
	if len(pos) > 0 {
		sub = pos[0]
	}
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		serviceUsage()
		return
	}

	switch sub {
	case "install":
		serviceInstall(args)
	case "uninstall", "remove":
		serviceUninstall(args)
	case "start":
		serviceControl(args, "start")
	case "stop":
		serviceControl(args, "stop")
	case "restart":
		serviceControl(args, "restart")
	case "status":
		serviceStatus()
	case "print", "template":
		servicePrint()
	default:
		serviceUsage()
	}
}

func serviceUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Printf(`Service — run the folder watcher in the background.

Usage:
  %s service install [--yes]   Install and activate the watcher
  %s service install --dry-run Show exactly what would be installed
  %s service print             Print the definitions without installing
  %s service status            Show what is installed and where
  %s service uninstall [--yes] Stop and remove the watcher
  %s service start|stop|restart [--yes]

Without --yes nothing is changed: you get the exact commands to run.
`, exe, exe, exe, exe, exe, exe)
}

// servicePrint shows the definitions and the install plan without writing
// anything, so the change can be reviewed before it happens.
func servicePrint() {
	exe, err := watcherExecutable()
	if err != nil {
		fatal("%v", err)
	}
	plan, err := planService(exe)
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("Tessera watcher service — %s\n\n", plan.Platform)
	fmt.Printf("Installs to: %s\n", plan.DefPath)
	if len(plan.BootstrapCmd) > 0 && !plan.BootstrapOptional {
		fmt.Println("Activates:   " + strings.Join(plan.BootstrapCmd, " "))
	}
	fmt.Println("Activates:   " + strings.Join(plan.InstallCmd, " "))
	fmt.Println()
	fmt.Println("── definition ────────────────────────────────────────────")
	if plan.Content == "" {
		fmt.Println("(configured directly through the platform command above)")
	} else {
		fmt.Print(plan.Content)
	}
	fmt.Println("──────────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("Install it with: tessera service install --yes")
}

// serviceInstall writes the definition, and activates it only with --yes.
func serviceInstall(args []string) {
	exe, err := watcherExecutable()
	if err != nil {
		fatal("%v", err)
	}
	plan, err := planService(exe)
	if err != nil {
		fatal("%v", err)
	}

	// Keep a reviewable copy regardless of whether we activate it.
	if dir, err := serviceDefinitionDir(); err == nil {
		if err := os.MkdirAll(dir, 0755); err == nil {
			if plan.Content != "" {
				_ = os.WriteFile(filepath.Join(dir, filepath.Base(plan.DefPath)), []byte(plan.Content), 0644)
			}
		}
	}

	activate := hasFlag(args, "--yes") || hasFlag(args, "-y")
	preview := hasFlag(args, "--dry-run")

	if preview {
		fmt.Printf("Would install to: %s\n", plan.DefPath)
		if plan.Content != "" {
			fmt.Println()
			fmt.Print(plan.Content)
		}
		fmt.Println()
		fmt.Println("Would run: " + strings.Join(plan.InstallCmd, " "))
		fmt.Println()
		fmt.Println("Nothing was changed (--dry-run).")
		return
	}

	if !activate {
		// Write the definition for review, then offer to activate it. Nothing
		// is started without an explicit yes (or --yes in a script).
		if err := installService(exe, plan, false); err != nil {
			fatal("%v", err)
		}
		fmt.Println()
		if yes, asked := promptYesNo("Start the watcher now, so your folders stay in sync?", true); asked {
			if yes {
				if err := installService(exe, plan, true); err != nil {
					fatal("%v", err)
				}
				fmt.Println("Watcher installed and started.")
				fmt.Println("Check it: tessera service status")
				return
			}
			fmt.Println("Not started. Start it later with: tessera service install --yes")
			return
		}
		fmt.Println("The watcher is NOT running yet.")
		fmt.Println()
		fmt.Println("  Activate it:  tessera service install --yes")
		fmt.Println("  Preview:      tessera service install --dry-run")
		fmt.Println()
		fmt.Println("Keeps every registered folder in sync in the background.")
		return
	}

	if err := installService(exe, plan, true); err != nil {
		fatal("%v", err)
	}
	fmt.Println()
	fmt.Println("Watcher installed and started.")
	if runtime.GOOS == "darwin" {
		fmt.Println("Logs: ~/.tessera/watch.log and ~/.tessera/watch.err")
	} else if runtime.GOOS == "linux" {
		fmt.Println("Logs: journalctl --user -u " + serviceUnit + " -f")
	}
	fmt.Println("Check it: tessera service status")
}

// serviceUninstall stops the watcher and removes the definition.
func serviceUninstall(args []string) {
	plan, err := planService("")
	if err != nil {
		fatal("%v", err)
	}
	if !hasFlag(args, "--yes") && !hasFlag(args, "-y") {
		fmt.Printf("Stop and remove the watcher service?\n")
		fmt.Printf("  %s\n", strings.Join(plan.RemoveCmd, " "))
		fmt.Println()
		fmt.Println("Re-run with --yes to proceed.")
		return
	}
	fmt.Println("Stopping:")
	if err := serviceRun(plan.RemoveCmd[0], plan.RemoveCmd[1:]...); err != nil {
		// Already stopped is not a failure worth aborting on.
		fmt.Printf("      (%v)\n", err)
	}
	if plan.Content != "" {
		if err := os.Remove(plan.DefPath); err != nil && !os.IsNotExist(err) {
			fatal("remove %s: %v", plan.DefPath, err)
		}
		fmt.Printf("Removed %s\n", plan.DefPath)
	}
	if dir, err := serviceDefinitionDir(); err == nil {
		_ = os.RemoveAll(dir)
	}
	fmt.Println("Watcher uninstalled. Your folders and files are untouched.")
}

// serviceControl wraps start/stop/restart around the platform command.
func serviceControl(args []string, action string) {
	plan, err := planService(mustWatcherExe())
	if err != nil {
		fatal("%v", err)
	}
	verb := action
	if action == "restart" {
		verb = "stop, then start"
	}
	if !hasFlag(args, "--yes") && !hasFlag(args, "-y") {
		fmt.Printf("This will %s the watcher service.\n", verb)
		fmt.Println("Re-run with --yes to proceed.")
		return
	}
	var cmds [][]string
	switch action {
	case "start":
		cmds = [][]string{plan.InstallCmd}
	case "stop":
		cmds = [][]string{plan.RemoveCmd}
	case "restart":
		cmds = [][]string{plan.RemoveCmd, plan.InstallCmd}
	}
	for _, c := range cmds {
		if err := serviceRun(c[0], c[1:]...); err != nil {
			fatal("%v", err)
		}
	}
	fmt.Printf("Watcher %sed.\n", action)
}

func mustWatcherExe() string {
	exe, err := watcherExecutable()
	if err != nil {
		return "tessera"
	}
	return exe
}

// serviceStatus reports whether the watcher is actually installed and running.
func serviceStatus() {
	plan, planErr := planService(mustWatcherExe())

	if planErr == nil && plan.Content != "" {
		if _, err := os.Stat(plan.DefPath); err == nil {
			fmt.Printf("Installed: %s\n", plan.DefPath)
		} else {
			fmt.Println("Not installed: no service definition found.")
		}
	} else if planErr == nil {
		fmt.Printf("Platform: %s (%s)\n", plan.Platform, plan.DefPath)
	} else {
		fmt.Printf("Platform: %s (automatic installation unsupported)\n", runtime.GOOS)
	}

	// Report real running state rather than just file existence.
	switch runtime.GOOS {
	case "darwin":
		out, err := serviceCommand("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+serviceLabel)
		if err == nil {
			fmt.Println("Running:   yes")
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "state = ") || strings.Contains(line, "pid = ") {
					fmt.Printf("           %s\n", strings.TrimSpace(line))
				}
			}
		} else {
			fmt.Println("Running:   no")
		}
	case "linux":
		out, err := serviceCommand("systemctl", "--user", "is-active", serviceUnit)
		state := strings.TrimSpace(string(out))
		if state == "" {
			state = "unknown"
		}
		if err != nil && state == "" {
			state = "inactive"
		}
		fmt.Printf("Running:   %s\n", state)
	default:
		fmt.Println("Running:   check with Task Manager (Windows)")
	}

	if dir, err := serviceDefinitionDir(); err == nil {
		if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
			fmt.Printf("\nReview copies in %s:\n", dir)
			for _, e := range ents {
				fmt.Printf("  %s\n", e.Name())
			}
		}
	}
	if planErr != nil {
		return
	}
	installed := false
	if plan.Content != "" {
		_, statErr := os.Stat(plan.DefPath)
		installed = statErr == nil
	}
	fmt.Println()
	if installed {
		fmt.Println("Restart:   tessera service restart --yes")
		fmt.Println("Remove:    tessera service uninstall --yes")
	} else {
		fmt.Println("Install:   tessera service install --yes")
		fmt.Println("Preview:   tessera service install --dry-run")
	}
}
