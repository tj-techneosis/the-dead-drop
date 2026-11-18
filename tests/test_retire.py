"""Retired boxes: mail to a box nobody reads any more is refused, saying
where to send it instead; what's already in it stays readable."""

import json

from _binary import DeadDrop, signers

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
        # What's in it stays readable, and can be filed (with the letter
        # saying it was retired).
        self.assertEqual(self.ids("old-desk")[0], kept)
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
        self.assertEqual((s["old-desk"]["retired"], s["old-desk"]["moved_to"], s["old-desk"]["unread"]), (True, "new-desk", 2))  # with the letter
        self.assertNotIn("retired", s["bob"])
        # Only the boxes asked about.
        self.assertNotIn("retired", self.dd("status", "--box", "bob").stdout)

    def test_a_damaged_list_lets_nothing_through(self):
        self.retire()
        (self.dir / "retired.json").write_text("{not json")
        p = self.dd("send", "--to", "bob", ok=False, input="hi\n")
        self.assertIn("won't read", p.stderr)
        self.assertFalse((self.dir / "bob").exists())



class RetireLetters(DeadDrop):
    """Retiring and unretiring tell the boxes concerned, from automation:
    the retired box, the box its mail goes to, and the store's me."""

    def letters(self, box):
        return [m for m in self.inbox(box) if m["from"] == "automation"]

    def test_retire_and_unretire_send_a_letter_to_each_box_concerned(self):
        self.config.write_text('me = "admin"\n')
        self.send("# Before\n\nx\n", "--to", "old-desk")
        self.me = "carol"
        self.dd("retire", "old-desk", "--moved-to", "new-desk", "--note", "the desk answers in new-desk now")
        line = "old-desk was retired by carol at 2026-11-02T15:30:00Z, moved to new-desk: the desk answers in new-desk now"
        for box in ("old-desk", "new-desk", "admin"):
            got = self.letters(box)
            self.assertEqual([m["subject"] for m in got], [line], box)
            self.assertIn("retired", got[0]["tags"])
        # The box stays retired to everyone else.
        self.assertEqual(self.dd("send", "--to", "old-desk", ok=False, input="hi\n").returncode, 5)
        self.clock = "2026-11-03T09:00:00Z"
        self.dd("unretire", "old-desk")
        back = "old-desk was unretired by carol at 2026-11-03T09:00:00Z"
        for box in ("old-desk", "new-desk", "admin"):
            self.assertEqual([m["subject"] for m in self.letters(box)][-1], back, box)

    def test_a_box_never_used_gets_no_letter_and_none_is_needed(self):
        self.dd("retire", "never-used")
        self.assertFalse((self.dir / "never-used").exists())
        self.dd("retire", "never-used-too", "--moved-to", "fresh")
        self.assertFalse((self.dir / "never-used-too").exists())
        self.assertEqual([m["subject"] for m in self.letters("fresh")],
                         ["never-used-too was retired by alice at 2026-11-02T15:30:00Z, moved to fresh"])

    def test_a_retired_me_gets_no_letter(self):
        self.config.write_text('me = "admin"\n')
        self.send("# Hi\n\nx\n", "--to", "admin")
        self.dd("retire", "admin", "--moved-to", "admin-two", "--force")
        self.send("# Hi\n\nx\n", "--to", "desk")
        self.dd("retire", "desk", "--moved-to", "new-desk")
        self.assertEqual(len(self.letters("admin")), 1)  # its own retiring, not desk's
        self.assertEqual(len(self.letters("desk")), 1)


class RetireOwners(DeadDrop):
    """An owner's main box isn't retired by accident: --force, and the
    letter says so."""

    def refused(self, box):
        p = self.dd("retire", box, "--moved-to", "elsewhere", ok=False)
        self.assertIn("owner's main box", p.stderr)
        self.assertIn("--force", p.stderr)
        self.assertFalse((self.dir / "retired.json").exists())
        self.assertFalse((self.dir / "elsewhere").exists())
        return p

    def test_a_box_named_in_owners_needs_force(self):
        self.config.write_text('owners = ["bob", "dora"]\n')
        self.send("# Hi\n\nx\n", "--to", "bob")
        self.assertIn("named in owners", self.refused("bob").stderr)
        self.dd("retire", "bob", "--moved-to", "elsewhere", "--force")
        self.assertEqual(self.dd("send", "--to", "bob", ok=False, input="hi\n").returncode, 5)
        want = "bob was retired by alice at 2026-11-02T15:30:00Z with --force, though it's an owner's main box, moved to elsewhere"
        for box in ("bob", "elsewhere"):
            self.assertIn(want, [m["subject"] for m in self.inbox(box)], box)
        # Any other box needs none.
        self.dd("retire", "desk")

    def test_the_stores_me_and_a_key_holder_need_force(self):
        self.config.write_text('me = "admin"\n')
        self.assertIn("the store's me", self.refused("admin").stderr)
        signers(self.tmp / "allowed_signers", "erin")
        p = self.dd("retire", "erin", ok=False, DEADDROP_SIGNERS=str(self.tmp / "allowed_signers"))
        self.assertIn("holds a key", p.stderr)
        self.dd("retire", "erin", "--force", DEADDROP_SIGNERS=str(self.tmp / "allowed_signers"))
        self.assertIn("retired", self.dd("status").stdout)

    def test_owners_is_checked(self):
        self.config.write_text('owners = ["Not A Box"]\n')
        self.assertIn("owners", self.dd("retire", "desk", ok=False).stderr)
