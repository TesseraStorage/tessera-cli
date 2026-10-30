package main

import (
	"os"
	"testing"
)

// TestMain keeps command tests network-free: signup's host-list warm-up
// would otherwise call the real indexer.
func TestMain(m *testing.M) {
	signupWarmHosts = func(*Config) {}
	os.Exit(m.Run())
}
