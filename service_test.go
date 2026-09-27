package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubServiceCommand replaces the command executor so tests can assert exactly
// what would run without touching the host.
func stubServiceCommand(t *testing.T) (*[][]string, func()) {
	t.Helper()
	orig := serviceCommand
	var calls [][]string
	serviceCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("stub ok"), nil
	}
	return &calls, func() { serviceCommand = orig }
}

// TestServiceDefinitionsAreBranded guards the branding regression: the
// definitions used to carry an unrelated vendor name, which also leaked into
// the install paths the user was told to copy from.
func TestServiceDefinitionsAreBranded(t *testing.T) {
	for _, forbidden := range []string{"siagate", "Siagate", "SIAGATE"} {
		if strings.Contains(plistContent("/usr/local/bin/tessera"), forbidden) {
			t.Errorf("plist still contains %q", forbidden)
		}
		if strings.Contains(unitContent("/usr/local/bin/tessera"), forbidden) {
			t.Errorf("systemd unit still contains %q", forbidden)
		}
		if strings.Contains(serviceLabel, forbidden) || strings.Contains(serviceUnit, forbidden) {
			t.Errorf("service identifier still contains %q", forbidden)
		}
	}
}

// TestPlistContent pins the launchd agent's required keys.
func TestPlistContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := plistContent("/opt/tessera")
	for _, want := range []string{
		"<key>Label</key><string>" + serviceLabel + "</string>",
		"<string>/opt/tessera</string><string>sync</string><string>watch</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
		filepath.Join(home, ".tessera", "watch.log"),
		filepath.Join(home, ".tessera", "watch.err"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q\n---\n%s", want, got)
		}
	}
}

// TestUnitContent pins the systemd unit's required directives, including a
// restart delay so a crash loop cannot spin.
func TestUnitContent(t *testing.T) {
	got := unitContent("/opt/tessera")
	for _, want := range []string{
		"ExecStart=/opt/tessera sync watch",
		"Restart=always",
		"RestartSec=5",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit is missing %q\n---\n%s", want, got)
		}
	}
}

// TestPlanServicePathsAreAbsolute checks the definition lands in the location
// the platform actually reads, and that the activation command references it.
func TestPlanServicePathsAreAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	plan, err := planService("/opt/tessera")
	if err != nil {
		t.Fatalf("planService: %v", err)
	}
	if !filepath.IsAbs(plan.DefPath) {
		t.Errorf("definition path %q is not absolute; a relative path would break the service", plan.DefPath)
	}
	if len(plan.InstallCmd) == 0 {
		t.Fatal("plan has no activation command")
	}
	switch runtime.GOOS {
	case "darwin":
		if !strings.HasSuffix(plan.DefPath, servicePlist) {
			t.Errorf("darwin definition path = %q, want it to end in %q", plan.DefPath, servicePlist)
		}
		if plan.InstallCmd[0] != "launchctl" {
			t.Errorf("darwin activation = %v, want launchctl", plan.InstallCmd)
		}
	case "linux":
		if !strings.HasSuffix(plan.DefPath, serviceUnit) {
			t.Errorf("linux definition path = %q, want it to end in %q", plan.DefPath, serviceUnit)
		}
		if plan.InstallCmd[0] != "systemctl" {
			t.Errorf("linux activation = %v, want systemctl", plan.InstallCmd)
		}
	}
}

// TestInstallServiceWritesDefinitionAndActivates is the core behaviour: with
// activation requested the definition is written and the platform command runs.
func TestInstallServiceWritesDefinitionAndActivates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	calls, restore := stubServiceCommand(t)
	defer restore()

	plan, err := planService("/opt/tessera")
	if err != nil {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skipf("platform %s is not auto-installable", runtime.GOOS)
		}
		t.Fatalf("planService: %v", err)
	}

	if err := installService("/opt/tessera", plan, true); err != nil {
		t.Fatalf("installService(activate): %v", err)
	}

	// The definition must exist at the platform path, not merely in scratch.
	if plan.Content != "" {
		data, err := os.ReadFile(plan.DefPath)
		if err != nil {
			t.Fatalf("definition not written to %s: %v", plan.DefPath, err)
		}
		if string(data) != plan.Content {
			t.Error("written definition differs from the rendered definition")
		}
	}

	// The activation command must have been run. On macOS a best-effort
	// bootout of any previous agent happens first, so search rather than
	// assume it is the only call.
	found := false
	for _, c := range *calls {
		if c[0] == plan.InstallCmd[0] && strings.Join(c, " ") == strings.Join(plan.InstallCmd, " ") {
			found = true
		}
	}
	if !found {
		t.Errorf("activation command %v was not executed; calls were %v", plan.InstallCmd, *calls)
	}
}

// TestInstallServiceWithoutActivationChangesNothing ensures the default path is
// inert: writing the definition is fine, starting a service is not.
func TestInstallServiceWithoutActivationChangesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	calls, restore := stubServiceCommand(t)
	defer restore()

	plan, err := planService("/opt/tessera")
	if err != nil {
		t.Skipf("platform %s is not auto-installable", runtime.GOOS)
	}
	if err := installService("/opt/tessera", plan, false); err != nil {
		t.Fatalf("installService(no activate): %v", err)
	}
	for _, c := range *calls {
		if c[0] == plan.InstallCmd[0] {
			t.Errorf("activation must not run without --yes, but ran %v", c)
		}
	}
}

// TestPromptYesNoNonInteractive covers the script path: without a terminal the
// prompt must be skipped rather than blocking forever on stdin.
func TestPromptYesNoNonInteractive(t *testing.T) {
	if stdinIsTerminal() {
		t.Skip("stdin is a terminal in this environment; the non-interactive path cannot be exercised")
	}
	answer, asked := promptYesNo("keep it in sync?", true)
	if asked {
		t.Error("asked must be false when stdin is not a terminal")
	}
	if answer {
		t.Error("answer must be false when no prompt was shown")
	}
}
