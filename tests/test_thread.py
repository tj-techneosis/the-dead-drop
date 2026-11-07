"""Threads and conversations: both sides of the mail in time order, as text,
JSON or a page, on the reader's machine."""

import json

from _binary import DeadDrop, at, key, script, signers


class Threads(DeadDrop):
    def setUp(self):
        super().setUp()
        self.signers = signers(self.tmp / "allowed_signers", "alice", "bob")

    def env(self, **extra):
        e = super().env(**extra)
        e.setdefault("DEADDROP_SIGNERS", str(self.signers))
        e.setdefault("DEADDROP_STATE", str(self.tmp / "state"))
        return e

    def conversation(self):
        """alice asks bob; bob answers as a persona; alice thanks him; plus mail
        that isn't part of it."""
        a = self.send("# Requests for tool X\n\nCan we get a dry-run flag?\n\n- one\n", "--to", "bob", "--tag", "tool-x")
        self.clock = at(1)
        self.dd("reply", a, "--as", "Release manager", input="Yes, next week.\n\nDetails follow.\n", DEADDROP_FROM="bob")
        b = self.ids(DEADDROP_FROM="alice")[0]
        self.clock = at(2)
        self.dd("reply", b, input="Great, thanks.\n")
        self.clock = at(3)
        self.send("# Unrelated\n\nOther topic.\n", "--to", "alice", DEADDROP_FROM="bob")
        self.send("# To carol\n\nx\n", "--to", "carol")
        self.send("# Carol to alice\n\nx\n", "--to", "alice", DEADDROP_FROM="carol")
        self.dd("mark", "read", "--tag", "tool-x", DEADDROP_FROM="bob")
        return a, b

    def test_a_thread_is_both_sides_in_order(self):
        a, b = self.conversation()
        c = json.loads(self.dd("thread", b, "--json").stdout)
        self.assertEqual(c["box"], "alice")
        ls = c["letters"]
        self.assertEqual([(l["mine"], l["subject"]) for l in ls],
                         [(True, "Requests for tool X"), (False, "Re: Requests for tool X"), (True, "Re: Requests for tool X")])
        self.assertEqual(ls[0]["gist"], "Can we get a dry-run flag?")
        self.assertEqual(ls[1]["persona"], "Release manager")
        self.assertEqual([x["state"] for x in ls[0]["copies"]], ["read"])
        # Asked by either side's copy id, the same thread.
        self.assertEqual(len(json.loads(self.dd("thread", a, "--json").stdout)["letters"]), 3)
        out = self.dd("thread", b).stdout
        self.assertIn("→ bob", out)
        self.assertIn("← bob (unverified), as Release manager", out)
        self.assertIn("seen", out)
        self.dd("mark", "unread", "--tag", "tool-x", DEADDROP_FROM="bob")
        self.assertIn("not yet read", self.dd("thread", b).stdout)
        self.dd("thread", "2026-11-02T153000-bob-ffff", ok=False)

    def test_with_is_everything_between_two_people(self):
        self.conversation()
        ls = json.loads(self.dd("with", "bob", "--json").stdout)["letters"]
        self.assertEqual([l["subject"] for l in ls], ["Requests for tool X", "Re: Requests for tool X", "Re: Requests for tool X", "Unrelated"])
        tagged = json.loads(self.dd("with", "bob", "--tag", "tool-x", "--json").stdout)["letters"]
        self.assertEqual(len(tagged), 3)
        self.assertEqual([l["subject"] for l in json.loads(self.dd("with", "carol", "--json").stdout)["letters"]],
                         ["To carol", "Carol to alice"])
        # Bob's view of the same conversation is his.
        mine = [l["mine"] for l in json.loads(self.dd("with", "alice", "--json", DEADDROP_FROM="bob").stdout)["letters"]]
        # Bob's persona was unsigned, so alice's thanks comes to bob's own
        # pigeonhole: only a verified persona routes a reply.
        self.assertEqual(mine, [False, True, False, True])

    def test_the_page(self):
        a, b = self.conversation()
        self.send("# <script>x</script>\n\n<img src=x onerror=alert(1)>\n", "--to", "alice", DEADDROP_FROM="bob")
        page = self.dd("with", "bob", "--html").stdout
        self.assertIn("default-src 'none'", page)
        self.assertEqual(page.count('<section class="letter mine'), 2)
        self.assertEqual(page.count('<section class="letter theirs'), 3)
        self.assertIn("<details><summary>The whole letter</summary>", page)
        self.assertIn("as Release manager", page)
        self.assertNotIn("<script>", page)
        self.assertNotIn("<img", page)
        self.assertIn("seen", page)

    def test_a_forged_letter_shows_as_forged(self):
        self.send("# From alice\n\nx\n", "--to", "bob", DEADDROP_SIGN_KEY=key("alice")[0])
        path = next((self.dir / "bob" / "new").glob("*.md"))
        path.write_text(path.read_text().replace("x\n", "y\n"))
        page = self.dd("with", "alice", "--html", DEADDROP_FROM="bob").stdout
        self.assertIn("letter theirs unread forged", page)
        self.assertIn("forged or changed", page)

    def test_open_shows_it_and_marks_nothing(self):
        a, b = self.conversation()
        opened = self.tmp / "opened"
        opener = script(self.tmp / "opener", f'cp "$1" "{opened}"\n')
        self.dd("open", "--thread", b, DEADDROP_OPENER=str(opener))
        self.assertIn("Requests for tool X", opened.read_text())
        self.dd("open", "--with", "bob", DEADDROP_OPENER=str(opener))
        self.assertIn("With bob", opened.read_text())
        self.assertEqual(len(self.ids(DEADDROP_FROM="alice")), 3)  # all still unread
        self.dd("open", "--with", "Not A Box", DEADDROP_OPENER=str(opener), ok=False)
        self.dd("open", b, "--thread", b, DEADDROP_OPENER=str(opener), ok=False)

    def test_the_menu_has_threads_and_conversations(self):
        self.conversation()
        self.dd("watch")
        out = self.dd("bar", "--format", "swiftbar").stdout
        self.assertIn('--Thread | bash=', out)
        self.assertIn('param1="open" param2="--thread"', out)
        self.assertIn("Conversations", out)
        self.assertIn('--With bob | bash=', out)
        self.assertIn('param2="--with" param3="carol"', out)

    def test_gist(self):
        self.send("# Title\n\n```\ncode first\n```\n\n## Why\n\n> The real point,\n> on two lines.\n\nMore.\n", "--to", "bob")
        l = json.loads(self.dd("with", "bob", "--json").stdout)["letters"][0]
        self.assertEqual(l["gist"], "The real point, on two lines.")

    def test_every_thread_on_one_page(self):
        a, b = self.conversation()
        page = self.dd("threads", "--html").stdout
        self.assertIn("default-src 'none'", page)
        self.assertEqual(page.count('<details class="topic'), 4)
        self.assertIn("Requests for tool X</span>", page)
        self.assertIn("The whole letter", page)  # the letters are inside
        self.assertNotIn("<script", page)
        opened = self.tmp / "opened"
        opener = script(self.tmp / "opener", f'cp "$1" "{opened}"\n')
        self.dd("open", "--threads", DEADDROP_OPENER=str(opener))
        self.assertIn("Threads</h1>", opened.read_text())
        self.assertEqual(len(self.ids(DEADDROP_FROM="alice")), 3)  # nothing marked
        self.dd("threads", "--html", "--json", ok=False)

    def test_asks_wait_until_answered(self):
        self.dd("send", "--to", "alice", "--ask", input="# Which name?\n\nWatches or Lookouts: which should the page say?\n\nDetail.\n", DEADDROP_FROM="bob")
        self.dd("send", "--to", "alice", "--tag", "ask-alice", input="# Old style\n\nMerge the release tonight?\n", DEADDROP_FROM="carol")
        self.send("# Just news\n\nx\n", "--to", "alice", DEADDROP_FROM="bob")
        asks = json.loads(self.dd("asks", "--json").stdout)
        self.assertEqual([a["question"] for a in asks], ["Watches or Lookouts: which should the page say?", "Merge the release tonight?"])
        self.assertIn("answer with deaddrop reply", self.dd("asks").stdout)
        self.dd("reply", asks[0]["id"], input="Lookouts.\n")
        self.assertEqual([a["subject"] for a in json.loads(self.dd("asks", "--json").stdout)], ["Old style"])
        self.dd("mark", "archive", asks[1]["id"])  # filed is closed too
        self.assertIn("Nothing waiting on your decision", self.dd("asks").stdout)
        # The answer doesn't carry the ask back.
        self.assertNotIn("ask", self.inbox(DEADDROP_FROM="bob")[0]["tags"])
        self.dd("send", "--to", "alice", "--ask", input="# Only a heading\n", DEADDROP_FROM="bob", ok=False)

    def test_an_ask_closes_only_on_a_real_answer(self):
        signers(self.tmp / "s3", "alice", "bob")
        env = dict(DEADDROP_SIGNERS=str(self.tmp / "s3"))
        mine = dict(DEADDROP_SIGN_KEY=key("alice")[0], **env)
        self.send("# Earlier\n\nx\n", "--to", "bob", **mine)  # alice signs
        self.dd("send", "--to", "alice", "--ask", input="# Q\n\nWhich one?\n", DEADDROP_FROM="bob", DEADDROP_SIGN_KEY=key("bob")[0], **env)
        ask = json.loads(self.dd("asks", "--json", **mine).stdout)[0]
        # A forged "answer" in alice's name doesn't close it.
        self.send("# Re: Q\n\nForged.\n", "--to", "bob", "--reply-to", ask["id"])  # unsigned, as alice
        self.assertEqual(len(json.loads(self.dd("asks", "--json", **mine).stdout)), 1)
        self.dd("reply", ask["id"], input="This one.\n", **mine)
        self.assertEqual(json.loads(self.dd("asks", "--json", **mine).stdout), [])
        # The answer carries neither ask nor ask-<box>.
        self.dd("send", "--to", "alice", "--tag", "ask-alice", input="# Q2\n\nAnd this?\n", DEADDROP_FROM="bob")
        q2 = [a for a in json.loads(self.dd("asks", "--json", **mine).stdout)][0]
        self.dd("reply", q2["id"], input="Yes.\n", **mine)
        back = [m for m in self.inbox(DEADDROP_FROM="bob", **env) if m["subject"] == "Re: Q2"][0]
        self.assertNotIn("ask-alice", back["tags"])

