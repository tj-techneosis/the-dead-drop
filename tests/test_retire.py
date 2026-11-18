"""Retired boxes: mail to a box nobody reads any more is refused, saying
where to send it instead; what's already in it stays readable."""

import json

from _binary import DeadDrop

# The same hint is checked over ssh and behind the gate (test_remote.py).
HINT = "old-desk is retired; the desk answers in new-desk now (send to new-desk)"


class Retire(DeadDrop):
    def retire(self):
        self.dd("retire", "old-desk", "--moved-to", "new-desk", "--note", "the desk answers in new-desk now")

    def test_a_send_to_a_retired_box_is_refused_and_writes_nothing(self):
        kept = self.send("# Before\n\nx\n", "--to", "old-desk")
        self.retire()
        before = sorted(str(p) for p in self.dir.rglob("*"))
        p = self.dd("send", "--to", "old-desk", ok=False, input="# After\n\nx\n")
        self.assertEqual(p.stderr.strip(), "deaddrop: " + HINT)
        self.assertEqual(p.returncode, 5)
        self.assertNotIn("usage:", p.stderr)
        self.assertEqual(sorted(str(p) for p in self.dir.rglob("*")), before)
        # What's in it stays readable, and can be filed.
        self.assertEqual(self.ids("old-desk"), [kept])
        self.assertIn("Before", self.dd("pull", "old-desk").stdout)
        self.assertIn("Before", self.dd("show", kept, "--box", "old-desk").stdout)
        self.dd("mark", "archive", kept, "--box", "old-desk")
        self.assertEqual(self.files("old-desk", "archive"), [kept])

    def test_a_send_to_several_is_refused_whole(self):
        self.retire()
        for to in (("--to", "bob", "--to", "old-desk"), ("--to", "bob,old-desk,carol")):
            p = self.dd("send", *to, "--for", "h1", ok=False, input="# Hi\n\nx\n")
            self.assertIn(HINT, p.stderr)
        for box in ("bob", "carol", "old-desk"):
            self.assertFalse((self.dir / box).exists(), box)

    def test_a_reply_to_someone_whose_box_is_retired_is_refused(self):
        self.me = "old-desk"
        orig = self.send("# A question\n\nx\n", "--to", "bob")
        self.me = "alice"
        self.retire()
        self.me = "bob"
        p = self.dd("reply", orig, ok=False, input="An answer.\n")
        self.assertIn(HINT, p.stderr)
        self.assertEqual(p.returncode, 5)
        self.assertFalse((self.dir / "old-desk").exists())

    def test_a_reply_box_that_is_retired_is_refused(self):
        self.retire()
        p = self.dd("send", "--to", "bob", "--reply-box", "old-desk", ok=False, input="# Hi\n\nx\n")
        self.assertIn("--reply-box: " + HINT, p.stderr)
        self.assertFalse((self.dir / "bob").exists())

    def test_unretire_takes_mail_again(self):
        self.retire()
        self.dd("unretire", "old-desk")
        self.send("# Back\n\nx\n", "--to", "old-desk")
        self.assertFalse((self.dir / "retired.json").exists())
        self.assertIn("isn't retired", self.dd("unretire", "old-desk", ok=False).stderr)

    def test_without_a_note_or_a_new_box(self):
        self.dd("retire", "gone")
        p = self.dd("send", "--to", "gone", ok=False, input="# Hi\n\nx\n")
        self.assertEqual(p.stderr.strip(), "deaddrop: gone is retired, and nothing takes its mail")
        self.dd("retire", "moved", "--moved-to", "here")
        p = self.dd("send", "--to", "moved", ok=False, input="# Hi\n\nx\n")
        self.assertEqual(p.stderr.strip(), "deaddrop: moved is retired (send to here)")

    def test_a_chain_points_at_the_box_that_is_read(self):
        self.dd("retire", "first", "--moved-to", "second")
        self.dd("retire", "second", "--moved-to", "third")
        self.assertIn("(send to third)", self.dd("send", "--to", "first", ok=False, input="hi\n").stderr)
        # Moving to a box that's retired says where its mail goes.
        p = self.dd("retire", "other", "--moved-to", "first", ok=False)
        self.assertIn("--moved-to third", p.stderr)

    def test_retire_is_checked(self):
        for args, said in ((("retire",), "one box"), (("retire", "me"), "in full"), (("retire", "automation"), "automation"),
                           (("retire", "Bad Name"), "not a name"), (("retire", "a", "--moved-to", "a"), "itself"),
                           (("retire", "a", "--note", "token ghp_" + "a" * 36), "secret")):
            self.assertIn(said, self.dd(*args, ok=False).stderr, args)
        self.assertFalse((self.dir / "retired.json").exists())

    def test_status_shows_retired_boxes_and_where_they_went(self):
        self.send("# Before\n\nx\n", "--to", "old-desk")
        self.send("# Hi\n\nx\n", "--to", "bob")
        self.retire()
        self.dd("retire", "never-used")
        out = self.dd("status").stdout
        self.assertIn("old-desk (retired)", out)
        self.assertIn("old-desk is retired, moved to new-desk: the desk answers in new-desk now", out)
        self.assertIn("never-used is retired", out)
        s = {b["box"]: b for b in json.loads(self.dd("status", "--json").stdout)}
        self.assertEqual((s["old-desk"]["retired"], s["old-desk"]["moved_to"], s["old-desk"]["unread"]), (True, "new-desk", 1))
        self.assertNotIn("retired", s["bob"])
        # Only the boxes asked about.
        self.assertNotIn("retired", self.dd("status", "--box", "bob").stdout)

    def test_a_damaged_list_lets_nothing_through(self):
        self.retire()
        (self.dir / "retired.json").write_text("{not json")
        p = self.dd("send", "--to", "bob", ok=False, input="hi\n")
        self.assertIn("won't read", p.stderr)
        self.assertFalse((self.dir / "bob").exists())

