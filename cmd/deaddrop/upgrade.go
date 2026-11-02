package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Upgrading is the installer's job, not this binary's, as it is for the
// Crontinental: `upgrade` runs the install.sh that came with the installed
// release, fed on stdin exactly as the one-liner feeds it, so it downloads,
// checks and installs the latest release the one way there is. All this side
// adds is knowing whether there is one.
//
// A release publishes VERSION beside its archives, so the latest is one small
// fetch from <releases>/latest/download/VERSION, the same against GitHub and
// against a file:// copy (DEADDROP_RELEASES, as for install.sh). curl does
// the fetching, as it does for the installer.

const defaultReleases = "https://github.com/tj-techneosis/the-dead-drop/releases"

const oneLiner = "curl -fsSL https://raw.githubusercontent.com/tj-techneosis/the-dead-drop/main/install.sh | bash"

// exitNewer is `upgrade --check`'s status when a newer release is out, so a
// scheduled check reads as a failure; 1 stays an error (no answer) and 0
// means nothing to do.
const exitNewer = 3

func releasesURL() string {
	if r := os.Getenv("DEADDROP_RELEASES"); r != "" {
		return strings.TrimRight(r, "/")
	}
	return defaultReleases
}

func latestRelease() (string, error) {
	if _, err := exec.LookPath("curl"); err != nil {
		return "", fmt.Errorf("upgrade needs curl, to ask the releases page")
	}
	url := releasesURL() + "/latest/download/VERSION"
	out, err := exec.Command("curl", "-fsSL", "--max-time", "30", url).Output()
	if err != nil {
		return "", fmt.Errorf("could not read the latest release from %s", url)
	}
	v := strings.TrimSpace(string(out))
	if _, _, ok := parseSemver(v); !ok {
		return "", fmt.Errorf("%s says the latest release is %q, which is not a version", url, v)
	}
	return v, nil
}

// parseSemver reads vMAJOR.MINOR.PATCH with an optional -prerelease.
func parseSemver(s string) (nums [3]int, pre string, ok bool) {
	core, pre, _ := strings.Cut(strings.TrimPrefix(s, "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 || !strings.HasPrefix(s, "v") {
		return nums, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, "", false
		}
		nums[i] = n
	}
	return nums, pre, true
}

// compareVersions is <0, 0 or >0 as a is older than, the same as, or newer
// than b. A prerelease comes before its release; two prereleases of the same
// version compare as text (rc1 < rc2).
func compareVersions(a, b string) int {
	an, ap, _ := parseSemver(a)
	bn, bp, _ := parseSemver(b)
	for i := range an {
		if an[i] != bn[i] {
			return an[i] - bn[i]
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return strings.Compare(ap, bp)
}

// released is this binary's version when it is a release, "" otherwise (a
// checkout build stamps whatever git describe said).
func released() string {
	if _, _, ok := parseSemver(version); ok {
		return version
	}
	return ""
}

// installer finds the install.sh this binary's release came with: install.sh
// keeps each release it installs in ~/.local/share/deaddrop-install/releases/<v>/.
func installer() (string, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share")
	}
	if v := released(); v != "" {
		script := filepath.Join(data, "deaddrop-install", "releases", v, "install.sh")
		if _, err := os.Stat(script); err == nil {
			return script, nil
		}
	}
	exe, _ := os.Executable()
	if root := filepath.Dir(filepath.Dir(exe)); fileExists(filepath.Join(root, "cmd", "deaddrop", "main.go")) {
		return "", fmt.Errorf("this deaddrop is built from the checkout at %s; upgrade it there (git pull, then ./install.sh)", root)
	}
	if released() == "" {
		return "", fmt.Errorf("this deaddrop is a build from a checkout (%s); upgrade it there (git pull, then ./install.sh), or install a release with:\n  %s", versionString(), oneLiner)
	}
	return "", fmt.Errorf("%s wasn't installed by install.sh, so there is nothing to upgrade it with; install a release with:\n  %s", exe, oneLiner)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func cmdUpgrade(args []string) error {
	const u = "upgrade [--check]"
	f, err := parseFlags(args, map[string]bool{"check": false})
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 0 {
		return badf(u, "upgrade takes nothing but --check")
	}
	latest, err := latestRelease()
	if err != nil {
		return err
	}
	this := released()
	if f.has("check") {
		switch {
		case this == "":
			fmt.Printf("this is a build from a checkout (%s); the latest release is %s\n", versionString(), latest)
		case compareVersions(latest, this) > 0:
			fmt.Printf("%s is out (this is %s); deaddrop upgrade installs it\n", latest, this)
			return exitError{exitNewer, ""}
		case compareVersions(latest, this) == 0:
			fmt.Printf("%s is the latest release\n", this)
		default:
			fmt.Printf("this is %s, ahead of the latest release (%s)\n", this, latest)
		}
		return nil
	}

	script, err := installer()
	if err != nil {
		return err
	}
	if compareVersions(latest, this) <= 0 {
		fmt.Printf("already up to date: this is %s, the latest release is %s\n", this, latest)
		return nil
	}
	fmt.Printf("upgrading from %s to %s\n", this, latest)
	// Piped in, the installer has no file of its own, so it downloads the
	// latest rather than reinstalling the release it sits in. Its own
	// options aren't passed: --no-claude and the rest were the first
	// install's choices, and the Claude Code step only updates what is there.
	in, err := os.Open(script)
	if err != nil {
		return err
	}
	defer in.Close()
	cmd := exec.Command("bash", "-s", "--")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() > 0 {
			return exitError{ee.ExitCode(), "the installer failed"}
		}
		return fmt.Errorf("could not run the installer: %v", err)
	}
	return nil
}
