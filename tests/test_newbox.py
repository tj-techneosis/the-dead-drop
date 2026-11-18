"""A box that has never existed: a send or reply to it is refused, naming
the boxes it likely meant, unless it says --new-box."""

from _binary import DeadDrop

STRICT = dict(DEADDROP_NEW_BOX=None)  # the harness starts boxes as it goes


class NewBox(DeadDrop):
    def setUp(self):
        super().setUp()
        for box in ("team-keeper", "carol", "bob"):
            self.send("# Hi\n\nx\n", "--to", box)

    def strict(self, *args, ok=False, input="# Hi\n\nx\n", **env):
        return self.dd(*args, ok=ok, input=input, **{**STRICT, **env})

    def test_a_short_name_is_refused_and_points_at_the_box(self):
        p = self.strict("send", "--to", "keeper")
        self.assertEqual(p.stderr.strip(), 'deaddrop: no box "keeper"; did you mean team-keeper? (--new-box starts keeper)')
        self.assertEqual(p.returncode, 4)
        self.assertFalse((self.dir / "keeper").exists())

    def test_a_typo_is_pointed_at_the_box_a_letter_away(self):
        self.assertIn("did you mean carol?", self.strict("send", "--to", "carl").stderr)
        p = self.strict("send", "--to", "zebedee")
        self.assertIn('no box "zebedee": nothing\'s ever been sent to it', p.stderr)
        self.assertNotIn("did you mean", p.stderr)

    def test_a_send_to_several_is_refused_whole(self):
        before = self.files("bob", "new")
        for to in (("--to", "bob", "--to", "keeper"), ("--to", "bob,keeper,carol")):
            p = self.strict("send", *to, "--for", "h1")
            self.assertIn('no box "keeper"', p.stderr)
            self.assertEqual(p.returncode, 4)
        self.assertEqual(self.files("bob", "new"), before)
        self.assertFalse((self.dir / "bob" / "for").exists())
        self.assertFalse((self.dir / "keeper").exists())

    def test_new_box_starts_one_on_purpose(self):
        self.strict("send", "--to", "keeper", "--new-box", ok=True)
        self.strict("send", "--to", "keeper", ok=True)  # it exists now
        self.assertEqual(len(self.files("keeper", "new")), 2)
        self.strict("send", "--to", "dora", ok=True, DEADDROP_NEW_BOX="1")  # a setup script's

    def test_your_own_box_needs_no_new_box(self):
        self.strict("send", "--to", "me", ok=True, DEADDROP_FROM="dave")
        self.assertEqual(len(self.files("dave", "new")), 1)

    def test_a_reply_needs_no_new_box_but_a_retired_box_still_refuses_it(self):
        orig = self.send("# A question\n\nx\n", "--to", "bob", DEADDROP_FROM="erin")
        self.me = "bob"
        self.strict("reply", orig, input="Yes.\n", ok=True)  # erin has never had mail
        self.assertEqual(len(self.files("erin", "new")), 1)
        self.assertIn("unknown flag --new-box", self.strict("reply", orig, "--new-box", input="Yes.\n").stderr)
        self.dd("retire", "erin", "--moved-to", "carol")
        p = self.strict("reply", orig, input="Again.\n")
        self.assertIn("erin is retired (send to carol)", p.stderr)
        self.assertEqual(p.returncode, 5)
        self.assertEqual(len(self.files("erin", "new")), 1)

    def test_a_signed_reply_needs_no_new_box_either(self):
        # Signed, a reply goes to the store as the send it makes.
        from _binary import key
        signing = lambda who: dict(DEADDROP_FROM=who, DEADDROP_SIGN_KEY=key(who)[0])
        orig = self.send("# A question\n\nx\n", "--to", "bob", **signing("frank"))
        self.strict("reply", orig, input="Yes.\n", ok=True, **signing("bob"))
        self.assertEqual(len(self.files("frank", "new")), 1)
        self.dd("retire", "frank")
        p = self.strict("reply", orig, input="Again.\n", **signing("bob"))
        self.assertIn("frank is retired", p.stderr)
        self.assertEqual(len(self.files("frank", "new")), 1)

    def test_retired_boxes_say_retired_and_are_never_a_hint(self):
        self.dd("retire", "team-keeper", "--moved-to", "keeper-desk")
        self.assertIn("team-keeper is retired", self.strict("send", "--to", "team-keeper").stderr)
        self.assertNotIn("team-keeper", self.strict("send", "--to", "keeper").stderr)
        self.assertIn("is retired", self.strict("send", "--to", "team-keeper", "--new-box").stderr)
