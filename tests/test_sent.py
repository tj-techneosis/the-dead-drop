"""deaddrop sent: what a person sent, across every box it went to, one entry
per message with each copy's state; and personas: one of a person's
assistants speaking for them, signed as part of the message."""

import json
import shutil

from _binary import DeadDrop, at, key, signers


class Sent(DeadDrop):
    def setUp(self):
        super().setUp()
        self.signers = signers(self.tmp / "allowed_signers", "alice", "bob")

    def env(self, **extra):
        e = super().env(**extra)
        e.setdefault("DEADDROP_SIGNERS", str(self.signers))
        return e

    def sent(self, *args, **env):
        return json.loads(self.dd("sent", *args, "--json", **env).stdout)

    def test_what_you_sent_and_nothing_else(self):
        a = self.send("# To bob\n\nfirst\n", "--to", "bob", "--tag", "tool-x")
        self.clock = at(1)
        b = self.send("# To both\n\nsecond\n", "--to", "bob,carol", "--tag", "tool-x", "--tag", "request")
        self.clock = at(2)
        self.send("# From bob\n\nnot alice's\n", "--to", "alice", DEADDROP_FROM="bob")
        self.send("# Bob to carol\n\nnot alice's\n", "--to", "carol", DEADDROP_FROM="bob")
        self.send("# A job\n\nx\n", "--to", "alice", "--from", "automation")
        ss = self.sent()
        self.assertEqual([s["subject"] for s in ss], ["To bob", "To both"])  # oldest first
        self.assertEqual(ss[0]["id"], a)
        self.assertEqual((ss[0]["state"], ss[0]["recipients"], ss[0]["body"]), ("sent", ["bob"], "# To bob\n\nfirst\n"))
        # One entry for a message that went to two people, with both copies.
        both = ss[1]
        self.assertEqual(both["recipients"], ["bob", "carol"])
        self.assertEqual([c["box"] for c in both["copies"]], ["bob", "carol"])
        self.assertIn(b, [c["id"] for c in both["copies"]])
        self.assertEqual({c["state"] for c in both["copies"]}, {"unread"})
        # Bob's sent is bob's.
        self.assertEqual([s["subject"] for s in self.sent(DEADDROP_FROM="bob")], ["From bob", "Bob to carol"])
        self.assertEqual(self.sent(DEADDROP_FROM="carol"), [])
        self.dd("sent", "--box", "automation", ok=False)

    def test_each_copys_state(self):
        self.send("# To both\n\nx\n", "--to", "bob,carol")
        ss = self.sent()
        bobs = [c["id"] for c in ss[0]["copies"] if c["box"] == "bob"][0]
        carols = [c["id"] for c in ss[0]["copies"] if c["box"] == "carol"][0]
        self.dd("mark", "read", bobs, DEADDROP_FROM="bob")
        self.dd("mark", "archive", carols, DEADDROP_FROM="carol")
        self.assertEqual({c["box"]: c["state"] for c in self.sent()[0]["copies"]}, {"bob": "read", "carol": "archived"})
        out = self.dd("sent").stdout
        self.assertIn("bob (read), carol (archived)", out)

    def test_filters(self):
        self.send("# A\n\nx\n", "--to", "bob", "--tag", "tool-x")
        self.clock = at(60 * 24 * 3)
        self.send("# B\n\nx\n", "--to", "carol", "--tag", "tool-x", "--tag", "request")
        self.send("# C\n\nx\n", "--to", "bob,carol", "--tag", "other")
        subjects = lambda *a: [s["subject"] for s in self.sent(*a)]
        self.assertEqual(subjects("--to", "bob"), ["A", "C"])
        self.assertEqual(subjects("--to", "carol", "--tag", "tool-x"), ["B"])
        self.assertEqual(subjects("--tag", "tool-x", "--tag", "request"), ["B"])
        self.assertEqual(subjects("--since", "1d"), ["B", "C"])
        self.assertIn("Nothing sent by alice to dave", self.dd("sent", "--to", "dave").stdout)

    def test_box_is_who_a_front_end_acts_as(self):
        self.send("# Mine\n\nx\n", "--to", "bob", DEADDROP_FROM="carol")
        self.assertEqual([s["subject"] for s in self.sent("--box", "carol")], ["Mine"])
        self.assertEqual(self.sent(), [])

    def test_old_unsigned_mail_to_several_still_comes_together(self):
        # Before 0.4 an unsigned message didn't record its recipients.
        self.send("# Old\n\nx\n", "--to", "bob,carol")
        for p in self.dir.glob("*/new/*.md"):
            p.write_text("".join(l for l in p.read_text().splitlines(True) if not l.startswith("recipients:")))
        ss = self.sent()
        self.assertEqual(len(ss), 1)
        self.assertEqual(ss[0]["recipients"], ["bob", "carol"])

    def test_signatures_and_forgeries(self):
        mine = dict(DEADDROP_SIGN_KEY=key("alice")[0])
        self.send("# Signed\n\nx\n", "--to", "bob,carol", **mine)
        ss = self.sent()
        self.assertEqual((ss[0]["signature"], len(ss[0]["copies"])), ("verified", 2))
        # A copy someone put in a third box doesn't make the real one look forged.
        p = next((self.dir / "bob" / "new").glob("*.md"))
        (self.dir / "dave" / "new").mkdir(parents=True)
        shutil.copy(p, self.dir / "dave" / "new" / p.name.replace("-alice-", "-alice-") )
        self.assertEqual(self.sent()[0]["signature"], "verified")
        # Something written in alice's name by someone else: there, but bad.
        self.send("# Forged\n\nx\n", "--to", "bob", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=key("bob")[0])
        forged = [s for s in self.sent() if s["subject"] == "Forged"][0]
        self.assertEqual(forged["signature"], "bad")
        self.assertIn("Someone else may have written it", self.dd("sent").stdout)


class Persona(DeadDrop):
    def setUp(self):
        super().setUp()
        self.signers = signers(self.tmp / "allowed_signers", "alice", "bob")

    def env(self, **extra):
        e = super().env(**extra)
        e.setdefault("DEADDROP_SIGNERS", str(self.signers))
        return e

    def test_a_persona_is_a_label_under_the_sender(self):
        id = self.send("# Status\n\nx\n", "--to", "bob", "--as", "Release manager", DEADDROP_SIGN_KEY=key("alice")[0])
        m = json.loads(self.dd("show", id, "--json", DEADDROP_FROM="bob").stdout)
        self.assertEqual((m["from"], m["persona"], m["signature"]), ("alice", "Release manager", "verified"))
        self.assertTrue(m["signature_note"].startswith("signed by alice, as Release manager, "), m["signature_note"])
        self.assertIn("alice, as Release manager", self.dd("inbox", DEADDROP_FROM="bob").stdout)
        self.assertIn('persona: "Release manager"', self.dd("pull", DEADDROP_FROM="bob").stdout)
        self.assertIn("as Release manager", self.dd("show", id, "--html", DEADDROP_FROM="bob").stdout)
        # Still alice's box, alice's sent.
        self.assertEqual(json.loads(self.dd("sent", "--json").stdout)[0]["persona"], "Release manager")

    def test_set_once_for_a_session(self):
        self.send("# A\n\nx\n", "--to", "bob", DEADDROP_AS="Night shift")
        self.send("# B\n\nx\n", "--to", "bob", "--as", "Day shift", DEADDROP_AS="Night shift")
        ps = {m["subject"]: m["persona"] for m in self.inbox("--read", DEADDROP_FROM="bob")}
        self.assertEqual(ps, {"A": "Night shift", "B": "Day shift"})

    def test_a_persona_cant_be_added_or_changed_or_taken_off(self):
        mine = dict(DEADDROP_SIGN_KEY=key("alice")[0])
        self.send("# With\n\nx\n", "--to", "bob", "--as", "Helper", **mine)
        self.send("# Without\n\nx\n", "--to", "carol", **mine)
        w = next((self.dir / "bob" / "new").glob("*.md"))
        o = next((self.dir / "carol" / "new").glob("*.md"))
        good_w, good_o = w.read_text(), o.read_text()
        for path, text in ((w, good_w.replace('persona: "Helper"', 'persona: "Boss"')),
                           (w, good_w.replace('persona: "Helper"\n', "")),
                           (o, good_o.replace("urgency: normal\n", 'urgency: normal\npersona: "Boss"\n'))):
            path.write_text(text)
            box = path.parent.parent.name
            self.assertEqual(self.inbox(box, "--read", DEADDROP_FROM=box)[0]["signature"], "bad")
        # Messages signed before personas still verify (no persona, same text).
        o.write_text(good_o)
        self.assertEqual(self.inbox("carol", DEADDROP_FROM="carol")[0]["signature"], "verified")

    def test_a_reply_can_have_one(self):
        id = self.send("# Q\n\nx\n", "--to", "bob", DEADDROP_SIGN_KEY=key("alice")[0])
        self.dd("reply", id, "--as", "Support desk", input="Yes.\n", DEADDROP_FROM="bob", DEADDROP_SIGN_KEY=key("bob")[0])
        m = self.inbox(DEADDROP_FROM="alice")[0]
        self.assertEqual((m["persona"], m["signature"]), ("Support desk", "verified"))
        # And unsigned.
        self.dd("reply", id, "--as", "Support desk", input="Again.\n", DEADDROP_FROM="bob")
        self.assertEqual({m["persona"] for m in self.inbox(DEADDROP_FROM="alice")}, {"Support desk"})

    def test_one_clean_line(self):
        id = self.send("# A\n\nx\n", "--to", "bob", "--as", "Line one\nline two " + "x" * 80)
        p = json.loads(self.dd("show", id, "--json", DEADDROP_FROM="bob").stdout)["persona"]
        self.assertNotIn("\n", p)
        self.assertLessEqual(len(p), 60)
        self.dd("send", "--to", "bob", "--as", " ", input="# B\n\nx\n", ok=False)


class ReplyTags(DeadDrop):
    def test_a_reply_keeps_the_topic_not_the_speaker(self):
        id = self.send("# Plan\n\nx\n", "--to", "bob", "--tag", "tool-x", "--tag", "proposal", "--tag", "persona-planner")
        self.dd("reply", id, "--tag", "persona-reviewer", "--tag", "status", "--untag", "proposal", input="Ok.\n", DEADDROP_FROM="bob")
        self.assertEqual(self.inbox()[0]["tags"], ["tool-x", "persona-reviewer", "status"])

    def test_the_same_when_signed(self):
        signers(self.tmp / "s", "alice", "bob")
        env = dict(DEADDROP_SIGNERS=str(self.tmp / "s"))
        id = self.send("# Plan\n\nx\n", "--to", "bob", "--tag", "topic", "--tag", "persona-a", DEADDROP_SIGN_KEY=key("alice")[0], **env)
        self.dd("reply", id, "--untag", "topic", "--tag", "persona-b", input="Ok.\n", DEADDROP_FROM="bob", DEADDROP_SIGN_KEY=key("bob")[0], **env)
        m = self.inbox(**env)[0]
        self.assertEqual((m["tags"], m["signature"]), (["persona-b"], "verified"))
        self.dd("reply", id, "--untag", "Not A Tag!", input="x\n", DEADDROP_FROM="bob", ok=False)


class SentAndReplays(DeadDrop):
    def test_a_planted_copy_doesnt_make_your_own_letter_bad(self):
        signers(self.tmp / "s", "alice")
        env = dict(DEADDROP_SIGNERS=str(self.tmp / "s"), DEADDROP_STATE=str(self.tmp / "state"))
        id = self.send("# Mine\n\nx\n", "--to", "bob", DEADDROP_SIGN_KEY=key("alice")[0], **env)
        src = next((self.dir / "bob" / "new").glob("*.md"))
        (src.parent / (id[:-4] + "aaaa.md")).write_text(src.read_text())
        ss = json.loads(self.dd("sent", "--json", **env).stdout)
        self.assertEqual([s["signature"] for s in ss], ["verified"])
