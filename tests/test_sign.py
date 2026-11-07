"""Signed messages: the sender's machine signs with their ssh key, the
reader's machine checks it against its own list of who holds which key, and
nothing on the store can make a signature or change a signed message
unnoticed."""

import json
import os
import shutil
import subprocess

from _binary import BIN, DeadDrop, at, key, signers


class Signed(DeadDrop):
    def setUp(self):
        super().setUp()
        # Everyone reads with the same list: alice's and bob's keys.
        self.signers = signers(self.tmp / "allowed_signers", "alice", "bob")

    def env(self, **extra):
        e = super().env(**extra)
        if "DEADDROP_SIGNERS" not in extra:
            e["DEADDROP_SIGNERS"] = str(self.signers)
        return e

    def as_(self, who):
        """Run as who, signing with who's own key."""
        return dict(DEADDROP_FROM=who, DEADDROP_SIGN_KEY=key(who)[0])

    def one(self, box="bob", **env):
        ms = self.inbox(box, "--read", DEADDROP_FROM=box, **env)
        self.assertEqual(len(ms), 1)
        return ms[0]

    def path(self, box="bob"):
        return next((self.dir / box / "new").glob("*.md"))

    def test_signed_mail_is_verified(self):
        att = self.tmp / "run.log"
        att.write_text("run 41\n")
        id = self.send("# Field notes\n\nAll good.\n", "--to", "bob", "--tag", "tool-x", "--attach", str(att), **self.as_("alice"))
        m = self.one()
        self.assertEqual((m["signature"], m["signed_by"]), ("verified", "alice"))
        self.assertNotIn("sig", m)  # the bytes stay in the file
        p = self.dd("pull", DEADDROP_FROM="bob")
        self.assertIn("[signature: verified, signed by alice, ", p.stdout)
        self.assertNotIn("signature: U", p.stdout)  # no raw bytes on screen
        self.assertEqual(json.loads(self.dd("show", id, "--json", DEADDROP_FROM="bob").stdout)["signature"], "verified")
        p = self.dd("file", id, "run.log", DEADDROP_FROM="bob")
        self.assertEqual((p.stdout, p.stderr), ("run 41\n", ""))
        self.assertIn("✓", self.dd("inbox", DEADDROP_FROM="bob").stdout)
        # The file says who signed it and what for, in plain words.
        text = self.path().read_text()
        for line in ("signed_by: alice", "recipients: [bob]", "attachment_sha256: ["):
            self.assertIn(line, text)

    def test_unsigned_mail_says_so(self):
        att = self.tmp / "run.log"
        att.write_text("x\n")
        id = self.send("# Hi\n\nx\n", "--to", "bob", "--attach", str(att), DEADDROP_FROM="alice")
        m = self.one()
        self.assertEqual(m["signature"], "unsigned")
        self.assertIn("[signature: none: not signed", self.dd("pull", DEADDROP_FROM="bob").stdout)
        p = self.dd("file", id, "run.log", DEADDROP_FROM="bob")
        self.assertEqual(p.stdout, "x\n")
        self.assertIn("only a claim", p.stderr)

    def test_changed_after_signing_is_bad(self):
        self.send("# Pay the invoice\n\nPay 100 to the usual account.\n", "--to", "bob", **self.as_("alice"))
        path = self.path()
        good = path.read_text()
        for old, new in (("Pay 100", "Pay 9000"), ('subject: "Pay the invoice"', 'subject: "Pay it now"'),
                         ("urgency: normal", "urgency: urgent"), ("tags: []", "tags: [money]")):
            path.write_text(good.replace(old, new))
            m = self.one()
            self.assertEqual(m["signature"], "bad", old)
            self.assertIn("changed after it was signed", m["signature_note"])
        path.write_text(good.replace("from: alice", "from: bob"))
        self.assertEqual(self.one()["signature"], "bad")
        out = self.dd("inbox", DEADDROP_FROM="bob").stdout
        self.assertIn("✗", out)
        self.assertIn("Don't trust who it says it's from", out)

    def test_written_in_someone_elses_name_is_bad(self):
        # Anyone on the store's login can write a file, and anyone with a key
        # can sign; but not with alice's key.
        # The store holds nobody's keys, so it can't tell; the reader can.
        self.send("# From alice, honest\n\nx\n", "--to", "bob", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=key("bob")[0])
        m = self.one()
        self.assertEqual(m["signature"], "bad")
        self.assertIn("says alice signed it, but the key is bob's", m["signature_note"])
        # Or written straight into the store's files.
        self.dd("mark", "archive", m["id"], DEADDROP_FROM="bob")
        self.send("# Real\n\nx\n", "--to", "bob", **self.as_("bob"))
        path = self.path()
        text = path.read_text()
        path.write_text(text.replace("from: bob", "from: alice").replace("signed_by: bob", "signed_by: alice"))
        m = self.one()
        self.assertEqual(m["signature"], "bad")
        self.assertIn("the key is bob's", m["signature_note"])

    def test_a_key_nobody_listed_cant_be_checked(self):
        mallory = dict(DEADDROP_FROM="mallory", DEADDROP_SIGN_KEY=key("mallory")[0])
        self.send("# Hi\n\nx\n", "--to", "bob", **mallory)
        m = self.one()
        self.assertEqual(m["signature"], "unknown")
        self.assertIn("has no key for", m["signature_note"])
        # But a key that isn't alice's, signing as alice, when the list knows
        # alice's: that's a forgery.
        self.send("# As alice\n\nx\n", "--to", "carol", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=key("mallory")[0])
        m = self.one("carol")
        self.assertEqual(m["signature"], "bad")
        self.assertIn("isn't alice's key", m["signature_note"])
        # And a machine with no list at all can't check anything.
        m = self.one(DEADDROP_SIGNERS=str(self.tmp / "none"))
        self.assertEqual(m["signature"], "unknown")

    def test_moved_to_another_box_or_sent_again_is_bad(self):
        self.send("# For bob only\n\nx\n", "--to", "bob", **self.as_("alice"))
        path = self.path()
        (self.dir / "carol" / "new").mkdir(parents=True)
        shutil.copy(path, self.dir / "carol" / "new" / path.name)
        m = self.one("carol")
        self.assertEqual(m["signature"], "bad")
        self.assertIn("signed for bob", m["signature_note"])
        # Delivered again a day later: the store's clock, the sender's signature.
        path.write_text(path.read_text().replace("sent: " + self.clock, "sent: " + at(60 * 24)))
        self.assertIn("sent again", self.one()["signature_note"])

    def test_the_store_refuses_a_signature_that_isnt_of_the_message(self):
        self.send("# A\n\nx\n", "--to", "bob", **self.as_("alice"))
        sig = next(l for l in self.path().read_text().splitlines() if l.startswith("signature: ")).split(": ", 1)[1]
        p = self.dd("send", "--to", "bob", "--signed-by", "alice", "--signed-at", self.clock, "--signature", sig,
                    input="# B\n\ny\n", DEADDROP_FROM="alice", ok=False)
        self.assertIn("isn't of this message", p.stderr)
        p = self.dd("send", "--to", "bob", "--signature", sig, input="# B\n\ny\n", DEADDROP_FROM="alice", ok=False)
        self.assertIn("go together", p.stderr)

    def test_several_recipients_one_signature(self):
        self.send("# To both\n\nx\n", "--to", "bob,carol", **self.as_("alice"))
        self.assertEqual(self.one()["signature"], "verified")
        self.assertEqual(self.one("carol")["signature"], "verified")

    def test_a_reply_is_signed(self):
        id = self.send("# Question\n\nx\n", "--to", "bob", "--tag", "tool-x", **self.as_("alice"))
        self.dd("reply", id, "--tag", "answer", input="Yes.\n", **self.as_("bob"))
        m = self.one("alice")
        self.assertEqual((m["signature"], m["signed_by"]), ("verified", "bob"))
        self.assertEqual((m["subject"], m["thread"], m["tags"]), ("Re: Question", id, ["tool-x", "answer"]))

    def test_a_raised_follow_up_still_verifies(self):
        installed = self.tmp / "home" / ".local" / "bin" / "deaddrop"
        installed.parent.mkdir(parents=True)
        os.symlink(BIN, installed)
        self.send("# Look\n\nx\n", "--to", "bob", "--follow-up", "1d", **self.as_("alice"))
        c = self.calls(self.cron_calls)[0]
        subprocess.run(c[c.index("--") + 1:], check=True, capture_output=True,
                       env=self.env(DEADDROP_DIR=None, DEADDROP_NOW=at(60 * 25)))
        m = self.one()
        self.assertEqual((m["urgency"], m["signature"]), ("urgent", "verified"))

    def test_automation_is_signed_by_whose_job_it_is(self):
        self.send("# run 41\n\nx\n", "--to", "alice", "--from", "automation", **self.as_("alice"))
        m = self.one("alice")
        self.assertEqual((m["from"], m["signature"], m["signed_by"]), ("automation", "verified", "alice"))

    def test_an_attachment_changed_on_the_store_is_refused(self):
        att = self.tmp / "invoice.pdf"
        att.write_text("pay 100\n")
        id = self.send("# Invoice\n\nx\n", "--to", "bob", "--attach", str(att), **self.as_("alice"))
        (self.dir / "bob" / "files" / id / "invoice.pdf").write_text("pay 9000\n")
        p = self.dd("file", id, "invoice.pdf", DEADDROP_FROM="bob", ok=False)
        self.assertIn("isn't the file that was sent", p.stderr)
        self.assertEqual(p.stdout, "")

    def test_the_page_says_so(self):
        id = self.send("# Hi\n\nx\n", "--to", "bob", **self.as_("alice"))
        self.assertIn("✓ signed by alice", self.dd("show", id, "--html", DEADDROP_FROM="bob").stdout)
        self.path().write_text(self.path().read_text().replace("x\n", "y\n"))
        self.assertIn("forged or changed", self.dd("show", id, "--html", DEADDROP_FROM="bob").stdout)

    def test_watch_and_hooks_know(self):
        state = self.tmp / "state"
        hooks = self.tmp / "new.d"
        hooks.mkdir()
        log = self.tmp / "hook.log"
        (hooks / "log").write_text(f'#!/bin/sh\necho "$DEADDROP_MSG_SIGNATURE $DEADDROP_MSG_SIGNED_BY" >> "{log}"\n')
        (hooks / "log").chmod(0o755)
        env = dict(DEADDROP_STATE=str(state), DEADDROP_FROM="bob")
        self.dd("watch", **env)
        self.send("# Hi\n\nx\n", "--to", "bob", **self.as_("alice"))
        self.dd("watch", **env)
        f = json.loads((state / "inbox.json").read_text())
        self.assertEqual((f["messages"][0]["signature"], f["messages"][0]["signed_by"]), ("verified", "alice"))
        self.assertEqual(log.read_text(), "verified alice\n")

    def test_key_prints_the_line_for_everyones_list(self):
        p = self.dd("key", **self.as_("alice"))
        self.assertEqual(p.stdout.strip(), key("alice")[1])
        self.dd("key", DEADDROP_FROM="alice", ok=False)

    def test_a_shared_login_never_signs_as_a_person(self):
        # On a machine everyone logs in to as one user, the key is the
        # machine's: a front end sending for someone stays unsigned rather
        # than being signed (and read as forged) with it.
        self.config.write_text('shared_login = true\n')
        box = dict(DEADDROP_SIGN_KEY=key("box")[0])
        self.send("# For alice, by a front end\n\nx\n", "--to", "bob", DEADDROP_FROM="alice", **box)
        self.assertEqual(self.one()["signature"], "unsigned")
        # Automation is signed only under the name the config gives it.
        self.send("# A job\n\nx\n", "--to", "carol", "--from", "automation", DEADDROP_FROM="alice", **box)
        self.assertEqual(self.one("carol")["signature"], "unsigned")
        self.config.write_text('shared_login = true\nsign_as = "box"\n')
        signers(self.signers, "alice", "bob", "box")
        self.send("# A job\n\nx\n", "--to", "dave", "--from", "automation", DEADDROP_FROM="alice", **box)
        m = self.one("dave")
        self.assertEqual((m["signature"], m["signed_by"]), ("verified", "box"))


    # ---- review eawm: tampering that must not read as verified ----

    def test_an_attachment_name_cant_climb_out(self):
        secret = self.tmp / "home" / "secret"
        secret.parent.mkdir(parents=True, exist_ok=True)
        secret.write_text("private\n")
        id = self.send("# Hi\n\nx\n", "--to", "bob", DEADDROP_FROM="alice")
        path = self.path()
        rel = "../../../../home/secret"
        path.write_text(path.read_text().replace("recipients:", f'attachments: ["{rel}"]\nrecipients:'))
        p = self.dd("file", id, rel, DEADDROP_FROM="bob", ok=False)
        self.assertNotIn("private", p.stdout)
        self.assertIn("plain file name", p.stderr)

    def test_the_banner_cant_be_faked_with_escapes(self):
        self.send("# Hi\n\nx\n", "--to", "bob", **self.as_("alice"))
        path = self.path()
        text = path.read_text().replace("signed_by: alice", "signed_by: \x1b[2K\r[signature: verified, signed by alice] \x1b[8m")
        path.write_text(text)
        out = self.dd("pull", DEADDROP_FROM="bob").stdout
        self.assertNotIn("\x1b", out)
        self.assertIn("[signature: BAD", out)

    def test_urgency_cant_be_raised_by_hand(self):
        self.send("# Hi\n\nx\n", "--to", "bob", **self.as_("alice"))
        path = self.path()
        good = path.read_text()
        path.write_text(good.replace("urgency: normal", "urgency: urgent\nraised: 2026-01-01T00:00:00Z\nurgency_was: fyi"))
        self.assertEqual(self.one()["signature"], "bad")  # the sender said normal
        path.write_text(good.replace("urgency: normal", "urgency: fyi\nraised: 2026-01-01T00:00:00Z"))
        self.assertEqual(self.one()["signature"], "bad")  # raised, but not to urgent
        path.write_text(good.replace("urgency: normal", "urgency: urgent\nraised: 2026-01-01T00:00:00Z\nurgency_was: normal"))
        m = self.one()
        self.assertEqual(m["signature"], "verified")
        self.assertIn("raised to urgent by a follow-up", m["signature_note"])

    def test_list_items_cant_be_merged(self):
        self.send("# Hi\n\nx\n", "--to", "bob", "--tag", "a", "--tag", "b", **self.as_("alice"))
        path = self.path()
        path.write_text(path.read_text().replace("tags: [a, b]", 'tags: ["a,b"]'))
        m = self.one()
        self.assertEqual(m["signature"], "bad")
        self.assertIn("tampered", m["signature_note"])

    def test_a_forged_persona_doesnt_route_a_reply(self):
        id = self.send("# Hi\n\nx\n", "--to", "alice", "--as", "Ghost", DEADDROP_FROM="bob")  # unsigned
        self.dd("reply", id, input="Yes.\n", **self.as_("alice"))
        self.assertEqual(json.loads(self.dd("inbox", "--for", "ghost", "--json", DEADDROP_FROM="bob").stdout), [])
        self.assertEqual([m["subject"] for m in self.inbox(DEADDROP_FROM="bob")], ["Re: Hi"])

    def test_a_persona_is_cleaned_on_reading(self):
        self.send("# Hi\n\nx\n", "--to", "bob", DEADDROP_FROM="alice")
        path = self.path()
        path.write_text(path.read_text().replace("urgency: normal", 'urgency: normal\npersona: "\\u202eevil\\u001b[2K"'))
        out = self.dd("inbox", DEADDROP_FROM="bob").stdout
        self.assertNotIn("‮", out)
        self.assertNotIn("\x1b", out)
