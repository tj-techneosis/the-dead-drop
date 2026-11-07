"""The wiring for a person's own machine: watch keeps the inbox file and runs
the new.d hooks once per new message; show hands out one message; open and bar
are what the bundled viewers use."""

import json
import os
import subprocess
import time

from _binary import BIN, DeadDrop, at, script


class Watch(DeadDrop):
    def setUp(self):
        super().setUp()
        self.state = self.tmp / "state"
        self.hooks = self.tmp / "new.d"
        self.hooks.mkdir()
        self.hook_log = self.tmp / "hook.log"
        # The hooks live beside the config.
        self.config = self.tmp / "config.toml"

    def env(self, **extra):
        e = super().env(**extra)
        if "DEADDROP_STATE" not in extra:
            e["DEADDROP_STATE"] = str(self.state)
        return e

    def hook(self, name="log", body=None):
        body = body or f'printf "%s|%s|%s|%s\\n" "$DEADDROP_MSG_ID" "$DEADDROP_MSG_SENDER" "$DEADDROP_MSG_SUBJECT" "$(cat)" >> "{self.hook_log}"\n'
        script(self.hooks / name, body)

    def fired(self):
        if not self.hook_log.exists():
            return []
        return [l.split("|", 3) for l in self.hook_log.read_text().splitlines()]

    def inbox_file(self):
        return json.loads((self.state / "inbox.json").read_text())

    def test_the_inbox_file(self):
        a = self.send("# Plain\n\nThe secret plans.\n", "--to", "alice", DEADDROP_FROM="bob")
        self.clock = at(1)
        b = self.send("# Fire\n\nbody\n", "--to", "alice", "--urgent", "--tag", "tool-x", DEADDROP_FROM="carol")
        self.clock = at(2)
        c = self.send("# Newest\n\nbody\n", "--to", "alice", DEADDROP_FROM="bob")
        d = self.send("# Read one\n\nbody\n", "--to", "alice", DEADDROP_FROM="bob")
        e = self.send("# Filed\n\nbody\n", "--to", "alice", DEADDROP_FROM="bob")
        self.send("# Someone else's\n\nbody\n", "--to", "bob")
        self.dd("mark", "read", d)
        self.dd("mark", "archive", e)
        p = self.dd("watch")
        self.assertIn("alice: 3 unread, 1 urgent", p.stdout)
        f = self.inbox_file()
        self.assertEqual(f["version"], 1)
        self.assertEqual(f["box"], "alice")
        self.assertTrue(f["ok"])
        self.assertEqual((f["unread"], f["urgent"]), (3, 1))
        self.assertEqual(f["checked_at"], at(2))
        # Unread first, urgent on top, newest first; read after; never archived.
        self.assertEqual([m["id"] for m in f["messages"]], [b, c, a, d])
        self.assertEqual(f["messages"][0]["from"], "carol")
        self.assertEqual(f["messages"][0]["tags"], ["tool-x"])
        self.assertEqual(f["messages"][3]["state"], "read")
        # Every field, always: a viewer never has to guess.
        for m in f["messages"]:
            self.assertEqual(set(m), {"id", "from", "subject", "tags", "urgency", "state", "sent", "thread", "attachments",
                                      "signature", "signature_note", "signed_by", "persona"})
            self.assertEqual(m["signature"], "unsigned")
        self.assertEqual((f["messages"][1]["attachments"], f["messages"][1]["thread"], f["messages"][1]["tags"]), ([], "", []))
        att = self.tmp / "build.log"
        att.write_text("x\n")
        self.send("# With a file\n\nx\n", "--to", "alice", "--reply-to", b, "--attach", str(att), DEADDROP_FROM="carol")
        self.dd("watch")
        top = [m for m in self.inbox_file()["messages"] if m["subject"] == "With a file"][0]
        self.assertEqual((top["attachments"], top["thread"]), (["build.log"], b))
        # Never the text.
        raw = (self.state / "inbox.json").read_text()
        self.assertNotIn("secret plans", raw)
        self.assertNotIn("body", raw)
        self.assertEqual(oct((self.state / "inbox.json").stat().st_mode & 0o777), "0o600")

    def test_hooks_fire_once_per_new_message(self):
        self.hook()
        old = self.send("# Old\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        # The first look announces nothing: it would be everything at once.
        self.assertIn("first look", self.dd("watch").stdout)
        self.assertEqual(self.fired(), [])
        new = self.send("# Requests for tool X\n\nthe text\n", "--to", "alice", "--tag", "tool-x", DEADDROP_FROM="bob")
        self.assertIn("1 new", self.dd("watch").stdout)
        self.dd("watch")  # a later pass, a new process: not again
        self.dd("watch")
        fired = self.fired()
        self.assertEqual(len(fired), 1)
        id, sender, subject, payload = fired[0]
        self.assertEqual((id, sender, subject), (new, "bob", "Requests for tool X"))
        j = json.loads(payload)
        self.assertEqual(j["id"], new)
        self.assertEqual(j["tags"], ["tool-x"])
        self.assertNotIn("the text", payload)
        self.assertNotEqual(id, old)

    def test_watch_box_keeps_another_box_in_its_own_file(self):
        self.hook(body=f'printf "%s|%s\\n" "$DEADDROP_MSG_ID" "$DEADDROP_MSG_BOX" >> "{self.hook_log}"\n')
        self.send("# Mine\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        self.dd("watch")
        # A box that isn't there is a typo, said, not an empty inbox.
        self.assertIn("no box alice-helper", self.dd("watch", "--box", "alice-helper", ok=False).stderr)
        self.send("# Before\n\nx\n", "--to", "alice-helper", DEADDROP_FROM="bob")
        self.assertIn("first look", self.dd("watch", "--box", "alice-helper").stdout)
        side = self.send("# For the side box\n\nx\n", "--to", "alice-helper", DEADDROP_FROM="bob")
        self.assertIn("1 new", self.dd("watch", "--box", "alice-helper").stdout)
        self.assertEqual(self.fired(), [[side, "alice-helper"]])
        f = json.loads((self.state / "inbox@alice-helper.json").read_text())
        self.assertEqual((f["box"], f["unread"]), ("alice-helper", 2))
        self.assertEqual(self.inbox_file()["box"], "alice")  # the caller's own file is untouched
        self.assertIn("alice-helper: 2 unread", self.dd("bar", "--box", "alice-helper").stdout)
        menu = self.dd("bar", "--box", "alice-helper", "--format", "swiftbar").stdout
        mark_lines = [l for l in menu.splitlines() if 'param1="mark"' in l]
        self.assertTrue(mark_lines and all('"--box" param' in l and '"alice-helper"' in l for l in mark_lines), menu)
        # --box me is the usual file; one box per watch.
        self.dd("watch", "--box", "me")
        self.assertFalse((self.state / "inbox@alice.json").exists())
        self.dd("watch", "--box", "a", "--box", "b", ok=False)

    def test_a_failing_hook_does_not_stop_the_others_or_refire(self):
        self.hook("a-broken", "echo broken >&2; exit 3\n")
        self.hook("b-log")
        script(self.hooks / ".hidden", f'echo hidden >> "{self.hook_log}"\n')
        self.dd("watch")
        self.send("# One\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        p = self.dd("watch")
        self.assertIn("hook a-broken", p.stderr)
        self.dd("watch")
        self.assertEqual(len(self.fired()), 1)
        self.assertNotIn("hidden", self.hook_log.read_text())

    def test_read_mail_is_not_announced(self):
        self.hook()
        self.dd("watch")
        id = self.send("# One\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        self.dd("mark", "read", id)
        self.dd("watch")
        self.dd("mark", "unread", id)  # it was seen: putting it back isn't news
        self.dd("watch")
        self.assertEqual(self.fired(), [])

    def test_no_hooks(self):
        self.hook()
        self.dd("watch")
        self.send("# One\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        self.dd("watch", "--no-hooks")
        self.assertEqual(self.fired(), [])

    def test_mark_brings_the_inbox_file_up_to_date(self):
        id = self.send("# One\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        self.dd("watch")
        self.assertEqual(self.inbox_file()["unread"], 1)
        self.dd("mark", "read", id)
        f = self.inbox_file()
        self.assertEqual(f["unread"], 0)
        self.assertEqual(f["messages"][0]["state"], "read")
        self.dd("mark", "archive", id)
        self.assertEqual(self.inbox_file()["messages"], [])

    def test_mark_leaves_someone_elses_inbox_file_alone(self):
        self.dd("watch")
        before = (self.state / "inbox.json").read_text()
        bobs = self.send("# For bob\n\nx\n", "--to", "bob")
        self.dd("mark", "read", bobs, DEADDROP_FROM="bob")
        self.assertEqual((self.state / "inbox.json").read_text(), before)

    def test_every(self):
        p = self.dd("watch", "--every", "2s", ok=False)
        self.assertIn("not more often", p.stderr)
        self.dd("watch", "--every", "soon", ok=False)
        self.send("# One\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        proc = subprocess.Popen([BIN, "watch", "--every", "10s"], env=self.env(), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(100):
                if (self.state / "inbox.json").exists():
                    break
                time.sleep(0.05)
            self.assertEqual(self.inbox_file()["unread"], 1)
            self.assertIsNone(proc.poll())  # still going
        finally:
            proc.terminate()
            proc.wait()

    # ---- show ----

    def test_show(self):
        id = self.send("# Requests\n\nThe whole text.\n", "--to", "alice", "--tag", "tool-x", DEADDROP_FROM="bob")
        p = self.dd("show", id)
        self.assertIn("The whole text.", p.stdout)
        self.assertIn("from: bob", p.stdout)
        j = json.loads(self.dd("show", id, "--json").stdout)
        self.assertEqual(j["body"], "# Requests\n\nThe whole text.\n")
        self.assertEqual(j["state"], "unread")
        # show marks nothing.
        self.assertEqual(self.ids(), [id])
        p = self.dd("show", "2026-11-02T153000-bob-ffff", ok=False)
        self.assertIn("no message", p.stderr)
        self.dd("show", ok=False)
        self.dd("show", id, "--box", "bob", ok=False)

    def test_show_html_makes_nothing_live(self):
        body = ("# Hello *there*\n\n"
                "<script>alert(1)</script>\n<img src=x onerror=alert(1)>\n\n"
                "[click](javascript:alert(1)) and [fine](https://example.com/?a=1&b=\"2\")\n\n"
                "![pixel](https://tracker.example/p.png)\n\n"
                "```\n<b>code</b>\n```\n\n- one\n- **two**\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")
        id = self.send(body, "--to", "alice", "--subject", "<b>Subject</b>", DEADDROP_FROM="bob")
        page = self.dd("show", id, "--html").stdout
        self.assertIn("default-src 'none'", page)
        self.assertNotIn("<script>", page)
        self.assertNotIn("<img", page)
        self.assertNotIn("javascript:", page.replace("(javascript:alert(1))", ""))
        self.assertNotIn('href="javascript', page)
        self.assertNotIn("tracker.example", page.split("<article>")[1].split("[pixel]")[0])
        self.assertNotIn("<b>", page)
        self.assertIn("&lt;b&gt;Subject&lt;/b&gt;", page)
        self.assertIn('<a href="https://example.com/?a=1&amp;b=&#34;2&#34;"', page)
        self.assertIn("<h1>Hello <em>there</em></h1>", page)
        self.assertIn("<li><strong>two</strong></li>", page)
        self.assertIn("<td>1</td>", page)
        self.assertIn("&lt;b&gt;code&lt;/b&gt;", page)
        frag = self.dd("show", id, "--html", "--fragment").stdout
        self.assertTrue(frag.startswith("<h1>Hello <em>there</em></h1>"), frag[:80])
        self.assertNotIn("<html", frag)
        self.assertNotIn("<img", frag)
        self.dd("show", id, "--fragment", ok=False)

    # ---- open ----

    def test_open_shows_it_and_marks_it_read(self):
        opened = self.tmp / "opened"
        opener = script(self.tmp / "opener", f'cp "$1" "{opened}"; echo "$1" >> "{opened}.path"\n')
        id = self.send("# Requests\n\nThe whole text.\n", "--to", "alice", DEADDROP_FROM="bob")
        self.dd("watch")
        self.dd("open", id, DEADDROP_OPENER=str(opener))
        self.assertIn("The whole text.", opened.read_text())
        page = opened.with_suffix(".path").read_text().strip()
        self.assertTrue(page.startswith(str(self.state)))
        self.assertEqual(oct(os.stat(page).st_mode & 0o777), "0o600")
        self.assertEqual(self.files("alice", "read"), [id])
        self.assertEqual(self.inbox_file()["unread"], 0)
        id2 = self.send("# Two\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        self.dd("open", id2, "--keep-unread", DEADDROP_OPENER=str(opener))
        self.assertEqual(self.files("alice", "new"), [id2])
        broken = script(self.tmp / "broken-opener", "exit 1\n")
        self.dd("open", id2, DEADDROP_OPENER=str(broken), ok=False)
        self.assertEqual(self.files("alice", "new"), [id2])  # not shown, so not read

    # ---- bar ----

    def test_bar(self):
        self.assertIn("Not watching yet", self.dd("bar").stdout)
        self.assertIn("Not watching yet", self.dd("bar", "--format", "swiftbar").stdout)
        self.dd("watch")
        self.assertEqual(self.dd("bar").stdout.strip(), "No mail.")
        self.send("x\n", "--to", "alice", "--urgent", "--subject", "a | b\n--- c", DEADDROP_FROM="bob")
        self.send("x\n", "--to", "alice", "--subject", "-- leading dashes", DEADDROP_FROM="bob")
        self.dd("watch")
        self.assertEqual(self.dd("bar").stdout.strip(), "1 urgent, 2 unread")
        out = self.dd("bar", "--format", "swiftbar").stdout
        lines = out.splitlines()
        self.assertTrue(lines[0].startswith("2 | sfimage=envelope.badge.fill"))
        self.assertIn("▲ bob (unsigned) · a ¦ b --- c | emojize=false", out)
        self.assertIn("● bob (unsigned) · leading dashes", out)
        self.assertIn('param1="open"', out)
        self.assertIn('param1="mark" param2="read"', out)
        # Every message line is one line, and its own: "|" only before params.
        for l in lines:
            if l.startswith(("▲", "●", "○")):
                self.assertEqual(l.count("|"), 1, l)
        self.dd("bar", "--format", "waybar", ok=False)
        # An old check says how old.
        self.assertIn("(as of 1h ago)", self.dd("bar", DEADDROP_NOW=at(60)).stdout)

