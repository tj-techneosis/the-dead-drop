"""Helpers: mail addressed --for one of a person's helpers lands in that
helper's own pigeonhole in the person's box, with its own read state, sealed
into the signature; replies find their way back; threads lists the topics."""

import json
import os
import shutil
import subprocess

from _binary import BIN, DeadDrop, at, key, script, signers


class Helpers(DeadDrop):
    def setUp(self):
        super().setUp()
        self.signers = signers(self.tmp / "allowed_signers", "alice", "bob")

    def env(self, **extra):
        e = super().env(**extra)
        e.setdefault("DEADDROP_SIGNERS", str(self.signers))
        e.setdefault("DEADDROP_STATE", str(self.tmp / "state"))
        return e

    def signed(self, who="alice"):
        return dict(DEADDROP_FROM=who, DEADDROP_SIGN_KEY=key(who)[0])

    def box(self, who, *args, **env):
        env.setdefault("DEADDROP_FROM", who)
        return self.inbox(*args, **env)

    def test_a_letter_for_a_helper(self):
        p = self.dd("send", "--to", "bob", "--for", "planner", input="# Deploy lock\n\nx\n", **self.signed())
        self.assertIn("to bob (for planner)", p.stdout)
        self.assertEqual(self.box("bob"), [])  # not bob's own pile
        m = self.box("bob", "--for", "planner")[0]
        self.assertEqual((m["for"], m["pigeonhole"], m["signature"]), (["planner"], "planner", "verified"))
        self.assertTrue(list((self.dir / "bob" / "for" / "planner" / "new").glob("*.md")))
        self.assertIn("No unread mail in reviewer's pigeonhole in bob's box", self.dd("inbox", "--for", "reviewer", DEADDROP_FROM="bob").stdout)

    def test_one_letter_two_helpers_own_state_each(self):
        self.send("# Shared\n\nx\n", "--to", "bob", "--for", "planner", "--for", "reviewer", **self.signed())
        k = self.box("bob", "--for", "planner")[0]
        self.dd("mark", "archive", k["id"], DEADDROP_FROM="bob")  # found in its pigeonhole by id
        self.assertEqual(self.box("bob", "--for", "planner"), [])
        r = self.box("bob", "--for", "reviewer")
        self.assertEqual([(m["subject"], m["state"], m["signature"]) for m in r], [("Shared", "unread", "verified")])
        s = json.loads(self.dd("sent", "--json").stdout)
        self.assertEqual(len(s), 1)
        self.assertEqual({(c["for"], c["state"]) for c in s[0]["copies"]}, {("planner", "archived"), ("reviewer", "unread")})
        self.assertEqual(s[0]["signature"], "verified")
        self.assertIn("bob/planner (archived), bob/reviewer (unread)", self.dd("sent").stdout)
        # Marking by tag stays in the pigeonhole asked for.
        self.dd("mark", "read", "--from", "alice", "--for", "reviewer", DEADDROP_FROM="bob")
        self.assertEqual(self.box("bob", "--read", "--for", "reviewer")[0]["state"], "read")

    def test_every_pigeonhole(self):
        self.send("# Mine\n\nx\n", "--to", "bob")
        self.send("# Planner's\n\nx\n", "--to", "bob", "--for", "planner")
        ms = self.box("bob", "--for", "*")
        self.assertEqual(sorted((m["subject"], m.get("pigeonhole", "")) for m in ms), [("Mine", ""), ("Planner's", "planner")])
        self.assertIn("unread (for planner)", self.dd("inbox", "--for", "*", DEADDROP_FROM="bob").stdout)

    def test_moved_between_pigeonholes_is_bad(self):
        self.send("# For the planner\n\nx\n", "--to", "bob", "--for", "planner", **self.signed())
        src = next((self.dir / "bob" / "for" / "planner" / "new").glob("*.md"))
        for dst in (self.dir / "bob" / "for" / "reviewer" / "new", self.dir / "bob" / "new"):
            dst.mkdir(parents=True, exist_ok=True)
            shutil.copy(src, dst / src.name)
        r = self.box("bob", "--for", "reviewer")[0]
        self.assertEqual(r["signature"], "bad")
        self.assertIn("signed for planner", r["signature_note"])
        self.assertEqual(self.box("bob")[0]["signature"], "bad")
        # And taking the address off, or putting one on.
        text = src.read_text()
        src.write_text(text.replace("for: [planner]\n", ""))
        self.assertEqual(self.box("bob", "--for", "planner")[0]["signature"], "bad")
        self.send("# Plain\n\nx\n", "--to", "carol", **self.signed())
        p = next((self.dir / "carol" / "new").glob("*.md"))
        p.write_text(p.read_text().replace("urgency: normal\n", "urgency: normal\nfor: [planner]\n"))
        self.assertEqual(self.box("carol")[0]["signature"], "bad")

    def test_replies_find_their_way_back(self):
        # Bob's helper writes as "Planner"; alice's answer goes to bob's planner.
        q = self.send("# A question\n\nx\n", "--to", "alice", "--as", "Planner", **self.signed("bob"))
        self.dd("reply", q, input="An answer.\n", **self.signed())
        self.assertEqual(self.box("bob"), [])
        a = self.box("bob", "--for", "planner")[0]
        self.assertEqual((a["subject"], a["for"], a["signature"]), ("Re: A question", ["planner"], "verified"))
        # The planner answers from its pigeonhole: found by id, sent as bob,
        # and the incoming "for" isn't carried back to alice.
        self.dd("reply", a["id"], "--as", "Planner", input="Thanks.\n", **self.signed("bob"))
        back = [m for m in self.box("alice") if m["subject"].startswith("Re:")]
        self.assertEqual([(m["subject"], m.get("for"), m["persona"]) for m in back], [("Re: A question", None, "Planner")])
        # --for picks the pigeonhole outright; a persona with no usable name goes to the person.
        self.dd("reply", q, "--for", "reviewer", input="To the reviewer.\n", **self.signed())
        self.assertEqual(len(self.box("bob", "--for", "reviewer")), 1)
        r = self.send("# Hi\n\nx\n", "--to", "alice", "--as", "!!!", DEADDROP_FROM="bob")
        self.dd("reply", r, input="Hi.\n")
        self.assertEqual([m["subject"] for m in self.box("bob")], ["Re: Hi"])

    def test_a_helper_sets_its_pigeonhole_once(self):
        self.send("# For the planner\n\nx\n", "--to", "bob", "--for", "planner")
        self.send("# For bob\n\nx\n", "--to", "bob")
        env = dict(DEADDROP_FROM="bob", DEADDROP_FOR="planner")
        self.assertEqual([m["subject"] for m in self.inbox(**env)], ["For the planner"])
        self.assertIn("For the planner", self.dd("pull", **env).stdout)
        self.dd("mark", "read", "--from", "alice", **env)
        self.assertEqual([m["subject"] for m in self.box("bob")], ["For bob"])  # bob's own untouched

    def test_watch_for_a_helper(self):
        hooks = self.tmp / "new.d"
        hooks.mkdir()
        log = self.tmp / "hook.log"
        script(hooks / "log", f'echo "$DEADDROP_MSG_FOR $DEADDROP_MSG_SUBJECT" >> "{log}"\n')
        bob = dict(DEADDROP_FROM="bob")
        self.dd("watch", "--for", "planner", **bob)
        self.dd("watch", **bob)
        self.send("# For the planner\n\nx\n", "--to", "bob", "--for", "planner")
        self.send("# For bob\n\nx\n", "--to", "bob")
        self.dd("watch", "--for", "planner", **bob)
        self.assertEqual(log.read_text(), "planner For the planner\n")
        f = json.loads((self.tmp / "state" / "inbox-planner.json").read_text())
        self.assertEqual((f["for"], [m["subject"] for m in f["messages"]]), ("planner", ["For the planner"]))
        self.assertEqual(self.dd("bar", "--for", "planner", **bob).stdout.strip(), "for planner: 1 unread")
        # mark --for brings the helper's file up to date.
        self.dd("mark", "read", "--from", "alice", "--for", "planner", **bob)
        self.assertEqual(json.loads((self.tmp / "state" / "inbox-planner.json").read_text())["unread"], 0)
        self.dd("watch", "--for", "*", ok=False, **bob)

    def test_gc_and_follow_ups_reach_the_pigeonholes(self):
        installed = self.tmp / "home" / ".local" / "bin" / "deaddrop"
        installed.parent.mkdir(parents=True)
        os.symlink(BIN, installed)
        self.send("# Look\n\nx\n", "--to", "bob", "--for", "planner", "--follow-up", "1d")
        c = self.calls(self.cron_calls)[0]
        subprocess.run(c[c.index("--") + 1:], check=True, capture_output=True, env=self.env(DEADDROP_DIR=None, DEADDROP_NOW=at(60 * 25)))
        self.assertEqual(self.box("bob", "--for", "planner")[0]["urgency"], "urgent")
        att = self.tmp / "a.txt"
        att.write_text("a\n")
        id = self.send("# Old\n\nx\n", "--to", "bob", "--for", "planner", "--attach", str(att))
        self.assertTrue((self.dir / "bob" / "for" / "planner" / "files" / id / "a.txt").exists())
        self.dd("mark", "archive", id, DEADDROP_FROM="bob")
        self.dd("gc", "--days", "1", DEADDROP_NOW=at(60 * 24 * 3))
        self.assertFalse((self.dir / "bob" / "for" / "planner" / "files" / id).exists())
        self.assertEqual(list((self.dir / "bob" / "for" / "planner" / "archive").glob("*.md")), [])


class Topics(DeadDrop):
    def test_threads_lists_the_topics(self):
        a = self.send("# Deploy lock\n\nx\n", "--to", "bob", "--tag", "proposal")
        self.clock = at(1)
        self.dd("reply", a, "--as", "Planner", "--untag", "proposal", "--tag", "status", input="Agreed.\n", DEADDROP_FROM="bob")
        self.clock = at(2)
        self.send("# Another topic\n\nlater\n", "--to", "bob", "--tag", "question")
        self.clock = at(3)
        self.send("# From carol\n\nx\n", "--to", "alice", DEADDROP_FROM="carol")
        t = json.loads(self.dd("threads", "--json").stdout)
        self.assertEqual(t["box"], "alice")
        topics = t["topics"]
        self.assertEqual([x["subject"] for x in topics], ["From carol", "Another topic", "Deploy lock"])  # newest first
        lock = topics[2]
        self.assertEqual((lock["root"], lock["count"], lock["unread"]), (a, 2, 1))
        self.assertEqual(lock["tags"], ["proposal", "status"])
        self.assertEqual({(p["box"], p.get("persona")) for p in lock["participants"]}, {("alice", None), ("bob", "Planner")})
        self.assertNotIn("letters", lock)
        full = json.loads(self.dd("threads", "--letters", "--json").stdout)["topics"][2]
        self.assertEqual([l["mine"] for l in full["letters"]], [True, False])
        self.assertNotIn("body", full["letters"][0])
        bodies = json.loads(self.dd("threads", "--letters", "--bodies", "--json").stdout)["topics"][2]
        self.assertEqual(bodies["letters"][1]["body"], "Agreed.\n")
        self.assertEqual(len(json.loads(self.dd("threads", "--limit", "1", "--json").stdout)["topics"]), 1)
        self.assertEqual(len(json.loads(self.dd("threads", "--since", "90s", "--json", DEADDROP_NOW=at(3)).stdout)["topics"]), 2)
        out = self.dd("threads").stdout
        self.assertIn("bob as Planner", out)
        self.dd("threads", "--bodies", ok=False)

    def test_a_reply_joins_the_thread_in_every_box(self):
        # alice writes to bob and carol; bob answers both from his own copy,
        # whose id carol's box doesn't have. carol still sees one topic.
        self.send("# Council\n\nvote please\n", "--to", "bob,carol", "--tag", "council")
        bobs = self.inbox(DEADDROP_FROM="bob")[0]["id"]
        self.clock = at(1)
        self.send("# Yes\n\nmy vote\n", "--to", "alice,carol", "--reply-to", bobs, "--tag", "vote", DEADDROP_FROM="bob")
        for who in ("carol", "alice", "bob"):
            topics = json.loads(self.dd("threads", "--json", DEADDROP_FROM=who).stdout)["topics"]
            self.assertEqual([(t["subject"], t["count"]) for t in topics], [("Council", 2)], who)
        carols = [m for m in self.inbox(DEADDROP_FROM="carol") if m["subject"] == "Council"][0]["id"]
        self.assertEqual(len(json.loads(self.dd("thread", carols, "--json", DEADDROP_FROM="carol").stdout)["letters"]), 2)

    def test_a_letter_to_yourself_shows_once_and_any_copy_finds_the_thread(self):
        self.send("# Council\n\nx\n", "--to", "alice,bob")
        mine = self.inbox()[0]["id"]
        letters = json.loads(self.dd("thread", mine, "--json").stdout)["letters"]
        self.assertEqual([l["mine"] for l in letters], [True])
        # bob asks by alice's copy's id, which isn't in his box.
        self.assertEqual(len(json.loads(self.dd("thread", mine, "--json", DEADDROP_FROM="bob").stdout)["letters"]), 1)
        self.dd("thread", "2026-11-02T153000-alice-ffff", DEADDROP_FROM="bob", ok=False)

    def test_open_a_helpers_thread(self):
        self.send("# For the planner\n\nx\n", "--to", "bob", "--for", "planner")
        id = self.inbox("--for", "planner", DEADDROP_FROM="bob")[0]["id"]
        opened = self.tmp / "opened"
        opener = script(self.tmp / "opener", f'cp "$1" "{opened}"\n')
        self.dd("open", "--thread", id, "--for", "planner", DEADDROP_FROM="bob", DEADDROP_OPENER=str(opener))
        self.assertIn("For the planner", opened.read_text())


class PigeonholeReview(DeadDrop):
    """Review eawm: what the pigeonholes got wrong."""

    def setUp(self):
        super().setUp()
        self.signers = signers(self.tmp / "allowed_signers", "alice", "bob")

    def env(self, **extra):
        e = super().env(**extra)
        e.setdefault("DEADDROP_SIGNERS", str(self.signers))
        e.setdefault("DEADDROP_STATE", str(self.tmp / "state"))
        return e

    def signed(self, who="alice"):
        return dict(DEADDROP_FROM=who, DEADDROP_SIGN_KEY=key(who)[0])

    def box(self, who, *args, **env):
        env.setdefault("DEADDROP_FROM", who)
        return self.inbox(*args, **env)

    def test_the_person_hears_about_helper_mail_and_typos(self):
        p = self.dd("send", "--to", "bob", "--for", "planer", input="# Typo\n\nx\n")
        self.assertIn("no planer pigeonhole yet", p.stderr)
        self.assertIn("unread for helpers: planer 1", self.dd("inbox", DEADDROP_FROM="bob").stdout)
        st = {s["box"]: s for s in json.loads(self.dd("status", "--json").stdout)}
        self.assertEqual(st["bob"]["helpers_unread"], {"planer": 1})
        self.assertIn("planer 1", self.dd("status").stdout)
        # Filed and gone: the empty pigeonhole goes with it.
        self.dd("mark", "archive", "--from", "alice", "--for", "planer", DEADDROP_FROM="bob")
        self.dd("gc", "--days", "1", DEADDROP_NOW=at(60 * 24 * 3))
        self.assertFalse((self.dir / "bob" / "for" / "planer").exists())

    def test_marking_by_id_refreshes_the_helpers_file(self):
        bob = dict(DEADDROP_FROM="bob")
        self.send("# For the planner\n\nx\n", "--to", "bob", "--for", "planner")
        self.dd("watch", "--for", "planner", **bob)
        id = self.inbox("--for", "planner", **bob)[0]["id"]
        self.dd("mark", "read", id, id, **bob)  # by id, twice: one move, no error
        f = json.loads((self.tmp / "state" / "inbox-planner.json").read_text())
        self.assertEqual(f["unread"], 0)
        self.assertFalse((self.tmp / "state" / "inbox.json").exists())  # the person's untouched

    def test_a_helper_session_replies_to_the_persons_mail(self):
        id = self.send("# To bob himself\n\nx\n", "--to", "bob", **self.signed("alice"))
        self.dd("reply", id, input="Yes.\n", DEADDROP_FOR="planner", **self.signed("bob"))
        self.assertEqual([m["subject"] for m in self.box("alice")], ["Re: To bob himself"])

    def test_a_letter_for_two_helpers_rings_once(self):
        installed = self.tmp / "home" / ".local" / "bin" / "deaddrop"
        installed.parent.mkdir(parents=True)
        os.symlink(BIN, installed)
        self.config.write_text("ring_urgent = true\n")
        self.send("# Look\n\nx\n", "--to", "bob", "--for", "planner", "--for", "reviewer", "--follow-up", "1d")
        jobs = [c for c in self.calls(self.cron_calls) if c[0] == "in"]
        self.assertEqual(len({c[c.index("--name") + 1] for c in jobs}), 2)  # a name each
        for c in jobs:
            subprocess.run(c[c.index("--") + 1:], check=True, capture_output=True, env=self.env(DEADDROP_DIR=None, DEADDROP_NOW=at(60 * 25)))
        rings = [c for c in self.calls(self.cron_calls) if c[0] == "notify"]
        self.assertEqual(len(rings), 1)
