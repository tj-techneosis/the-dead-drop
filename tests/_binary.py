"""The binary under test, and a throwaway store to run it against.

DEADDROP_BIN points the suite at any build of the CLI: the tests are
black-box on purpose, so they are the conformance suite for any
implementation. Unset, the checkout is built once into a temp directory.
"""

import atexit
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest

# The trust checks judge folders by their mode, and a umask of 0002 (common on
# Linux) makes the tests' own folders group-writable, which they'd rightly
# refuse. The suite runs under 022, as a person's own machine usually does.
os.umask(0o022)
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def _build() -> str:
    out = tempfile.mkdtemp(prefix="deaddrop-test-")
    atexit.register(shutil.rmtree, out, ignore_errors=True)
    path = os.path.join(out, "deaddrop")
    subprocess.run(["go", "build", "-o", path, "./cmd/deaddrop"], cwd=ROOT, check=True,
                   env={**os.environ, "CGO_ENABLED": "0"})
    return path


BIN = os.environ.get("DEADDROP_BIN") or _build()

T0 = "2026-11-02T15:30:00Z"


def at(minutes: float) -> str:
    """T0 plus some minutes, as DEADDROP_NOW wants it."""
    from datetime import datetime, timedelta, timezone
    t = datetime(2026, 11, 2, 15, 30, tzinfo=timezone.utc) + timedelta(minutes=minutes)
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def script(path: Path, body: str) -> Path:
    path.write_text("#!/bin/sh\n" + body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)
    return path


_KEYS = {}


def key(name: str) -> tuple:
    """An ssh key of name's own (made once), and its allowed_signers line."""
    if name not in _KEYS:
        d = tempfile.mkdtemp(prefix="deaddrop-key-")
        atexit.register(shutil.rmtree, d, ignore_errors=True)
        path = os.path.join(d, "id_" + name)
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path], check=True)
        typ, pub = Path(path + ".pub").read_text().split()[:2]
        _KEYS[name] = (path, f'{name} namespaces="deaddrop" {typ} {pub}')
    return _KEYS[name]


def signers(path: Path, *names, **as_) -> Path:
    """An allowed_signers file: each name with their own key, and any
    as_={principal: whose key} lines on top."""
    lines = [key(n)[1] for n in names]
    for principal, who in as_.items():
        lines.append(principal + key(who)[1][len(who):])
    path.write_text("\n".join(lines) + "\n")
    return path


class DeadDrop(unittest.TestCase):
    """Each test gets its own store, no config, a pinned clock, and stand-ins
    for Crontinental and the Firetower that write down what they were asked."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="deaddrop-case-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.dir = self.tmp / "store"
        self.config = self.tmp / "config.toml"
        self.clock = T0
        self.me = "alice"
        self.cron_calls = self.tmp / "crontinental-calls"
        self.tower_calls = self.tmp / "firetower-calls"
        self.cron = self.standin("crontinental", self.cron_calls)
        self.tower = self.standin("firetower", self.tower_calls)

    def standin(self, name, calls):
        """A command that writes each call's arguments to a file of its own."""
        calls.mkdir()
        return script(self.tmp / name, f'n=$(ls "{calls}" | wc -l | tr -d " "); printf "%s\\n" "$@" > "{calls}/$(printf %04d $n)"\n')

    def env(self, **extra):
        e = {k: v for k, v in os.environ.items() if not k.startswith(("DEADDROP_", "CRONTINENTAL_"))}
        # A HOME of its own, so no deaddrop installed on this machine is seen.
        (self.tmp / "home").mkdir(exist_ok=True)
        e.update(HOME=str(self.tmp / "home"), DEADDROP_DIR=str(self.dir), DEADDROP_CONFIG=str(self.config), DEADDROP_NOW=self.clock,
                 DEADDROP_FROM=self.me, DEADDROP_CRONTINENTAL=str(self.cron), DEADDROP_FIRETOWER=str(self.tower))
        for k, v in extra.items():
            if v is None:
                e.pop(k, None)
            else:
                e[k] = v
        return e

    def dd(self, *args, ok=True, input=None, timeout=30, **env):
        p = subprocess.run([BIN, *args], capture_output=True, text=True, env=self.env(**env),
                           input=input if input is not None else "", timeout=timeout)
        if ok is True and p.returncode != 0:
            self.fail(f"deaddrop {' '.join(args)} exited {p.returncode}:\n{p.stdout}{p.stderr}")
        if ok is False and p.returncode == 0:
            self.fail(f"deaddrop {' '.join(args)} should have failed:\n{p.stdout}")
        return p

    def send(self, body="# A note\n\nHello.\n", *args, **env):
        """Sends, and returns the new message's id."""
        p = self.dd("send", *args, input=body, **env)
        return p.stdout.split()[1]

    def inbox(self, *args, **env):
        return json.loads(self.dd("inbox", *args, "--json", **env).stdout)

    def ids(self, *args, **env):
        return [m["id"] for m in self.inbox(*args, **env)]

    def calls(self, calls):
        """Each call a stand-in got, as its argument list, in order."""
        return [p.read_text().splitlines() for p in sorted(calls.iterdir())]

    def files(self, box, state):
        return sorted(p.stem for p in (self.dir / box / state).glob("*.md"))
