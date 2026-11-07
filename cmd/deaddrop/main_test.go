package main

import (
	"os"
	"syscall"
	"testing"
)

// The tests make folders the trust checks judge by their mode; a machine
// whose umask leaves them group-writable (0002, common on Linux) would fail
// them for the right reason. So the tests run under 022, as a person's own
// machine usually does.
func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	os.Exit(m.Run())
}
