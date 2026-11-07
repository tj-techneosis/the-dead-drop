"""deaddrop wait: block until there's mail no wait has reported, then a line
per letter; what's reported is kept per box, never seeded from a snapshot."""

import json
import subprocess
import time

from _binary import BIN, DeadDrop


class Wait(DeadDrop):
    def wait(self, *args, **env):
        return self.dd("wait", *args, timeout=10, DEADDROP_FROM="bob", **env)

    def blocks(self, *args, **env):
        """Whether a wait is still waiting after a moment (then it's stopped)."""
        env.setdefault("DEADDROP_FROM", "bob")
        p = subprocess.Popen([BIN, "wait", *args], env=self.env(**env),
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(1.5)
            return p.poll() is None
        finally:
            p.kill()
            p.communicate()

    def test_mail_already_unread_is_reported_at_once_without_its_text(self):
        a = self.send("# First\n\nthe secret body\n", "--to", "bob", "--tag", "tool-x", "--urgent")
        p = self.wait()
        self.assertEqual(len(p.stdout.splitlines()), 1)
        line = p.stdout
        for part in (a, "urgent", "from alice", "[unsigned]", "[tool-x]", "First"):
            self.assertIn(part, line)
        self.assertNotIn("secret body", line)

    def test_a_letter_landing_between_two_waits_is_reported(self):
        # The gap: one wait ends, mail lands before the next starts. Kept
        # state, not a snapshot at start, so it's still news.
        a = self.send("# A\n\nx\n", "--to", "bob")
        self.assertIn(a, self.wait().stdout)
        b = self.send("# B\n\nx\n", "--to", "bob")
        p = self.wait()
        self.assertIn(b, p.stdout)
        self.assertNotIn(a, p.stdout)  # reported already, and still unread: not again

    def test_nothing_new_keeps_waiting(self):
        self.send("# A\n\nx\n", "--to", "bob")
        self.wait()
        self.assertTrue(self.blocks())
        self.assertIn("A", self.wait("--again").stdout)  # --again: everything unread

    def test_read_then_unread_again_is_new_again(self):
        a = self.send("# A\n\nx\n", "--to", "bob")
        self.wait()
        self.dd("mark", "read", a, DEADDROP_FROM="bob")
        self.assertTrue(self.blocks())  # a look while it's read lets it go
        self.dd("mark", "unread", a, DEADDROP_FROM="bob")
        self.assertIn(a, self.wait().stdout)

    def test_alive_names_the_box_and_json_has_no_text(self):
        self.send("# A\n\nthe body\n", "--to", "bob", "--for", "bob-helper")
        alive = self.tmp / "alive"
        p = self.wait("--for", "bob-helper", "--alive", str(alive), "--json")
        self.assertEqual(alive.read_text(), "bob bob-helper\n")
        ms = json.loads(p.stdout)
        self.assertEqual([m["subject"] for m in ms], ["A"])
        self.assertNotIn("the body", p.stdout)

    def test_another_box_that_isnt_there_exits_4(self):
        p = self.dd("wait", "--box", "carol", ok=False, timeout=10, DEADDROP_FROM="bob")
        self.assertEqual(p.returncode, 4)
        self.assertIn("no box carol", p.stderr)

    def test_two_names_sharing_a_state_folder_keep_their_own(self):
        a = self.send("# For bob\n\nx\n", "--to", "bob")
        c = self.send("# For carol\n\nx\n", "--to", "carol")
        self.assertIn(a, self.wait().stdout)
        self.assertIn(c, self.dd("wait", timeout=10, DEADDROP_FROM="carol").stdout)
        self.assertTrue(self.blocks())  # bob's set survived carol's wait
        self.assertTrue(self.blocks(DEADDROP_FROM="carol"))

    def test_a_look_older_than_the_state_never_drops_from_it(self):
        # Another wait saved after this one's fetch began: its letters may be
        # newer than this fetch, so they're kept, not pruned.
        import datetime
        state = self.tmp / "state"
        a = self.send("# A\n\nx\n", "--to", "bob")
        self.wait(DEADDROP_STATE=str(state))
        f = state / "waited@bob.json"
        w = json.loads(f.read_text())
        soon = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=30)).strftime("%Y-%m-%dT%H:%M:%SZ")
        w.update(reported=[a, "2026-11-02T153000-carol-later"], saved_at=soon)
        f.write_text(json.dumps(w))
        self.assertTrue(self.blocks(DEADDROP_STATE=str(state)))
        self.assertIn("2026-11-02T153000-carol-later", json.loads(f.read_text())["reported"])
        # One saved long ago is pruned to what's still unread.
        w.update(saved_at="2026-01-01T00:00:00Z")
        f.write_text(json.dumps(w))
        self.assertTrue(self.blocks(DEADDROP_STATE=str(state)))
        self.assertEqual(json.loads(f.read_text())["reported"], [a])

    def test_alive_in_a_folder_not_made_yet_is_made(self):
        self.send("# A\n\nx\n", "--to", "bob")
        alive = self.tmp / "fresh" / "dir" / "alive"
        self.wait("--alive", str(alive))
        self.assertEqual(alive.read_text(), "bob\n")

    def test_an_alive_path_that_cant_be_written_fails_at_the_start(self):
        ro = self.tmp / "ro-alive"
        ro.mkdir()
        ro.chmod(0o500)
        self.addCleanup(ro.chmod, 0o700)
        p = self.dd("wait", "--alive", str(ro / "alive"), ok=False, timeout=10, DEADDROP_FROM="bob")
        self.assertEqual(p.returncode, 2)
        self.assertIn("can't write there", p.stderr)

    def test_a_state_that_cant_be_kept_is_said(self):
        self.send("# A\n\nx\n", "--to", "bob")
        ro = self.tmp / "ro"
        ro.mkdir()
        ro.chmod(0o500)
        try:
            p = self.dd("wait", ok=False, timeout=10, DEADDROP_FROM="bob", DEADDROP_STATE=str(ro / "state"))
            self.assertIn("the wait's state", p.stderr)
        finally:
            ro.chmod(0o700)

    def test_a_save_lost_after_the_print_reports_again_never_loses(self):
        # As a kill between print and save: printed first, so nothing's lost,
        # and the next wait says it again.
        state = self.tmp / "state"
        self.send("# A\n\nx\n", "--to", "bob")
        self.wait(DEADDROP_STATE=str(state))
        b = self.send("# B\n\nx\n", "--to", "bob")
        state.chmod(0o500)
        try:
            p = self.wait(DEADDROP_STATE=str(state))
        finally:
            state.chmod(0o700)
        self.assertIn(b, p.stdout)
        self.assertIn("next wait may report these again", p.stderr)
        self.assertIn(b, self.wait(DEADDROP_STATE=str(state)).stdout)

    def test_each_box_keeps_its_own_reported(self):
        a = self.send("# For bob\n\nx\n", "--to", "bob")
        c = self.send("# For carol\n\nx\n", "--to", "carol")
        self.assertIn(a, self.wait().stdout)
        self.assertIn(c, self.wait("--box", "carol").stdout)
        self.assertTrue(self.blocks())  # bob's still reported


if __name__ == "__main__":
    import unittest
    unittest.main()
