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
    def run_hook(self, *args, payload='{"session_id":"s1"}', shell="sh", **env):
        e = self.env(DEADDROP_BIN=str(BIN), DEADDROP_STATE=str(self.tmp / "state"), **env)
        e["DEADDROP_FROM"] = e.get("DEADDROP_FROM") or "alice"
        p = subprocess.run([shell, str(HOOK), *args], input=payload, capture_output=True, text=True, env=e, timeout=30)
        self.assertEqual(p.returncode, 0, p.stderr)
        return p

    def hook(self, event="UserPromptSubmit", **kw):
        p = self.run_hook(event, **kw)
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

    def test_a_box_with_an_underscore_and_mixed_case(self):
        self.send("x\n", "--to", "tj_helper", DEADDROP_FROM="bob")
        self.assertIn("in tj_helper", self.hook(DEADDROP_BOX=" TJ_Helper ")["additionalContext"])

    def test_an_old_setting_without_the_event_still_speaks(self):
        self.send("x\n", "--to", "alice", DEADDROP_FROM="bob")
        p = self.run_hook()
        self.assertIn("1 unread letter", p.stdout)  # plain text: context on UserPromptSubmit
        self.assertIn("give the event", p.stderr)

    def test_a_long_payload_is_read_to_the_end(self):
        big = '{"session_id":"s9","tool_response":"' + "x" * 300000 + '"}'
        self.assertIsNone(self.hook(event="PostToolUse", payload=big))  # no mail, and no broken pipe

    def test_a_helpers_session_hears_its_own_pigeonhole(self):
        self.send("x\n", "--to", "alice", "--for", "planner", DEADDROP_FROM="bob")
        out = self.hook(DEADDROP_FOR="planner")
        self.assertIn("(for planner)", out["additionalContext"])
        self.assertIn("--for planner", out["additionalContext"])
        self.assertEqual(self.dd("status", "--box", "alice", "--for", "planner", "--count").stdout, "1 0\n")

    def test_a_memo_from_the_future_or_a_link_doesnt_silence_it(self):
        self.send("x\n", "--to", "alice", DEADDROP_FROM="bob")
        state = self.tmp / "state"
        state.mkdir(parents=True, exist_ok=True)
        (state / "hook-me-s1").write_text("1 0 9999999999\n")  # good digits, a time ahead
        self.assertIsNotNone(self.hook())
        (state / "hook-me-s1").unlink()
        (self.tmp / "elsewhere").write_text("1 0 9999999999\n")
        os.symlink(self.tmp / "elsewhere", state / "hook-me-s1")
        self.assertIsNotNone(self.hook())
        self.assertFalse((state / "hook-me-s1").is_symlink())

    def test_an_urgent_letter_is_heard_at_once(self):
        self.send("x\n", "--to", "alice", DEADDROP_FROM="bob")
        self.hook()
        self.dd("mark", "read", "--from", "bob")
        self.send("y\n", "--to", "alice", "--urgent", DEADDROP_FROM="bob")  # same unread count, now urgent
        self.assertIn("(1 urgent)", self.hook()["additionalContext"])
