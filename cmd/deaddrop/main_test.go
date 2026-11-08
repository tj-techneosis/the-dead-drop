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
	// A machine's pinned list of signers would be the only one checked, so
	// on one that has it every signature in a test would read as unknown.
	// The tests' own lists stand in, as the black-box suite's build does.
	systemSigners = "/nonexistent/deaddrop-test/allowed_signers"
	os.Exit(m.Run())
}
