package main

import (
	"strings"
	"testing"
)

// The version must come from the build (injected from the release tag), so a
// release tag and the binary's reported version can never drift apart.
func TestCmdVersionReportsInjectedVersion(t *testing.T) {
	orig := version
	defer func() { version = orig }()

	version = "v1.2.3"
	out := captureStdout(t, cmdVersion)
	if got, want := strings.TrimSpace(out), "tessera v1.2.3"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestCmdVersionDefaultsToDev(t *testing.T) {
	// The declared default is "dev" (no ldflags), so a plain build is obvious.
	if version != "dev" {
		// Only meaningful when the test binary itself was built without
		// ldflags; CI's `go test` does not set them.
		t.Skipf("version was injected as %q; default not exercised", version)
	}
	out := captureStdout(t, cmdVersion)
	if got, want := strings.TrimSpace(out), "tessera dev"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
