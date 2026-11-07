"""The Claude Code hook in examples/claude-code: it tells a session its
unread count, and nothing anyone sends or plants can steer what it says or
run anything."""

import json
import os
import subprocess
from pathlib import Path

from _binary import BIN, DeadDrop, at

HOOK = Path(__file__).resolve().parent.parent / "examples" / "claude-code" / "unread-mail"


class Hook(DeadDrop):
    def hook(self, event="UserPromptSubmit", payload='{"session_id":"s1"}', shell="sh", **env):
        e = self.env(DEADDROP_BIN=str(BIN), DEADDROP_STATE=str(self.tmp / "state"), **env)
        e["DEADDROP_FROM"] = e.get("DEADDROP_FROM") or "alice"
        p = subprocess.run([shell, str(HOOK), event], input=payload, capture_output=True, text=True, env=e, timeout=30)
        self.assertEqual(p.returncode, 0, p.stderr)
        return json.loads(p.stdout)["hookSpecificOutput"] if p.stdout.strip() else None

    def test_status_count_is_two_bare_numbers(self):
        self.send("x\n", "--to", "alice", "--urgent", DEADDROP_FROM="bob")
        self.send("y\n", "--to", "alice", DEADDROP_FROM="bob")
        self.assertEqual(self.dd("status", "--box", "alice", "--count").stdout, "2 1\n")
        self.dd("status", "--count", ok=False)  # one box, named

    def test_it_says_the_count_and_nothing_else(self):
        self.assertIsNone(self.hook())  # no mail: quiet
        self.send("# A secret subject\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        out = self.hook()
        self.assertEqual(out["hookEventName"], "UserPromptSubmit")
        self.assertIn("1 unread letter in your box", out["additionalContext"])
        self.assertNotIn("secret", out["additionalContext"])
        self.assertIsNone(self.hook())  # the same count, soon after: quiet
        self.send("z\n", "--to", "alice", DEADDROP_FROM="bob")
        self.assertIn("2 unread letters", self.hook()["additionalContext"])  # it rose: says so
        self.assertIsNotNone(self.hook(payload='{"session_id":"s2"}'))  # another session hears it too
        out = self.hook(event="PostToolUse", payload='{"session_id":"s3"}')
        self.assertEqual(out["hookEventName"], "PostToolUse")
        self.assertIsNone(self.hook(event="Stop"))  # only the events it's for

    def test_a_pigeonhole_named_unread_cant_set_the_count(self):
        for _ in range(3):
            self.send("x\n", "--to", "alice", "--for", "unread", DEADDROP_FROM="bob")
        self.send("y\n", "--to", "alice", DEADDROP_FROM="bob")
        self.assertIn("1 unread letter", self.hook()["additionalContext"])

    def test_the_sessions_own_box(self):
        self.send("x\n", "--to", "alice-helper", DEADDROP_FROM="bob")
        out = self.hook(DEADDROP_REPLY_BOX="alice-helper")
        self.assertIn("in alice-helper", out["additionalContext"])

    def test_a_planted_memo_is_ignored_and_runs_nothing(self):
        self.send("x\n", "--to", "alice", DEADDROP_FROM="bob")
        state = self.tmp / "state"
        state.mkdir(parents=True, exist_ok=True)
        canary = self.tmp / "ran"
        for shell in ("sh", "bash"):
            (state / "hook-me-s1").write_text(f"a[$(touch {canary})] 0 9999999999\n")
            out = self.hook(shell=shell)
            self.assertFalse(canary.exists(), f"{shell} ran the memo")
            self.assertIsNotNone(out)  # a memo it can't read doesn't silence it
            self.assertEqual(oct((state / "hook-me-s1").stat().st_mode & 0o777), "0o600")
