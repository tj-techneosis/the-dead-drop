# Keeping the inbox file current

`deaddrop watch` checks your box once, updates
`~/.local/state/deaddrop/inbox.json` and runs your `new.d` hooks for new
mail. Run it every minute or so with whatever your machine has. Pick one.

**Crontinental** (any machine):

```sh
crontinental every 1m --name deaddrop-watch --forever --notify never -- deaddrop watch
```

**macOS, no scheduler:** `launchd` runs it every minute. Replace HOME_DIR in
`deaddrop-watch.plist`, then:

```sh
cp deaddrop-watch.plist ~/Library/LaunchAgents/com.example.deaddrop-watch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.deaddrop-watch.plist
```

**Linux, systemd:** a service that keeps running and checks every minute:

```sh
cp deaddrop-watch.service ~/.config/systemd/user/
systemctl --user enable --now deaddrop-watch
```

**Anything else** (cron, a login script, a terminal tab):

```sh
* * * * * $HOME/.local/bin/deaddrop watch >/dev/null 2>&1
deaddrop watch --every 1m
```

Your config has to say who you are (`me = "alice"`), since none of these
run with your shell's `DEADDROP_FROM`.
