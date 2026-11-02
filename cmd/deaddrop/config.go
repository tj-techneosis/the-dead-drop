package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is ~/.config/deaddrop/config.toml (or $DEADDROP_CONFIG). Most
// people need none on the store's machine; elsewhere it says where the store
// is and who you are.
type Config struct {
	Me         string `toml:"me"`          // who you are, when the login name won't do
	Host       string `toml:"host"`        // the store's machine, for ssh; empty: the store is here
	RemoteBin  string `toml:"remote_bin"`  // deaddrop on that machine (default: ~/.local/bin/deaddrop, else PATH)
	Dir        string `toml:"dir"`         // the store, on this machine
	RingUrgent bool   `toml:"ring_urgent"` // urgent mail also rings crontinental notify
	// SharedLogin: everyone logs in to this machine as the same user, so the
	// login name says nothing about who is asking and is never used.
	SharedLogin bool `toml:"shared_login"`

	path string
}

func configPath() string {
	if p := os.Getenv("DEADDROP_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "deaddrop", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "deaddrop", "config.toml")
}

var cachedConfig *Config

// loadConfig reads the config once. A missing file is no config; a broken
// one is an error, since guessing the store or the sender would put mail in
// the wrong place.
func loadConfig() (*Config, error) {
	if cachedConfig != nil {
		return cachedConfig, nil
	}
	c := &Config{path: configPath()}
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			cachedConfig = c
			return c, nil
		}
		return nil, err
	}
	md, err := toml.Decode(string(b), c)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", c.path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown setting %s", c.path, strings.Join(keys, ", "))
	}
	if c.Me != "" {
		if err := checkBox("me", c.Me); err != nil {
			return nil, fmt.Errorf("%s: %v", c.path, err)
		}
	}
	cachedConfig = c
	return c, nil
}

// storeDir is where the boxes are, on this machine.
func storeDir() (string, error) {
	if d := os.Getenv("DEADDROP_DIR"); d != "" {
		return d, nil
	}
	c, err := loadConfig()
	if err != nil {
		return "", err
	}
	if c.Dir != "" {
		return expandHome(c.Dir), nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "deaddrop"), nil
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "deaddrop"), nil
}

// explicitMe is who the caller said they are ($DEADDROP_FROM, or me in the
// config), or "" when they didn't say.
func explicitMe() (string, error) {
	if v := os.Getenv("DEADDROP_FROM"); v != "" {
		if err := checkBox("$DEADDROP_FROM", v); err != nil {
			return "", err
		}
		return v, nil
	}
	c, err := loadConfig()
	if err != nil {
		return "", err
	}
	return c.Me, nil
}

// whoAmI is the caller: explicitMe, else the login name (unless the login
// is shared).
func whoAmI() (string, error) {
	me, err := explicitMe()
	if err != nil || me != "" {
		return me, err
	}
	if c, _ := loadConfig(); c != nil && c.SharedLogin {
		return "", fmt.Errorf("this machine's login is shared (shared_login in %s), so say who you are: set DEADDROP_FROM", c.path)
	}
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	name = strings.ToLower(name)
	if checkBox("", name) != nil {
		return "", fmt.Errorf("the login name %q won't do as a box name; set DEADDROP_FROM (or me in %s)", name, configPath())
	}
	return name, nil
}

// resolveBox turns "me" (or nothing) into the caller, and checks the rest.
func resolveBox(what, who string) (string, error) {
	if who == "" || who == "me" {
		return whoAmI()
	}
	who = strings.ToLower(who)
	if err := checkBox(what, who); err != nil {
		return "", err
	}
	if who == automation {
		return "", fmt.Errorf("%s: automation is a sender, not a box (an automation sends --to its owner)", what)
	}
	return who, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
