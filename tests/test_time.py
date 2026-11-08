"""What the Dead Drop hands to other tools: the Firetower lookouts, the
follow-up jobs it books with Crontinental, and the optional ring."""

import json
import os
import subprocess

from _binary import BIN, DeadDrop, at, script


class Firetower(DeadDrop):
    def reports(self):
        return {c[1]: c for c in self.calls(self.tower_calls)}

    def test_clear_smoke_fire_and_moved(self):
        # Build order step 4.
        self.send("x\n", "--to", "empty")
        self.dd("mark", "read", "--from", "alice", "--box", "empty")
        self.send("a\n", "--to", "bob")
        first = self.send("b\n", "--to", "carol", "--urgent")
        self.clock = at(1)
        second = self.send("c\n", "--to", "carol")
        self.dd("firetower")
        r = self.reports()
        self.assertEqual(sorted(r), ["mail/bob/inbox", "mail/carol/inbox", "mail/empty/inbox"])
        self.assertEqual(r["mail/empty/inbox"][2:4], ["green", "No mail."])
        self.assertEqual(r["mail/bob/inbox"][2:4], ["yellow", "1 unread"])
        self.assertEqual(r["mail/carol/inbox"][2:4], ["red", "1 urgent, 2 unread"])
        c = r["mail/carol/inbox"]
        # The value stands for the newest message, without naming its sender.
        value = c[c.index("--value") + 1]
        self.assertRegex(value, r"^[0-9a-f]{12}$")
        self.assertNotIn("alice", value)
        self.clock = at(2)
        self.send("d\n", "--to", "carol")
        self.dd("firetower", "--box", "carol")
        c2 = self.calls(self.tower_calls)[-1]
        self.assertNotEqual(c2[c2.index("--value") + 1], value)  # new mail: moved
        self.dd("firetower", "--box", "carol")
        c3 = self.calls(self.tower_calls)[-1]
        self.assertEqual(c3[c3.index("--value") + 1], c2[c2.index("--value") + 1])  # nothing new: still
        self.assertEqual(r["mail/empty/inbox"][r["mail/empty/inbox"].index("--value") + 1], "none")
        self.assertNotIn("--next", c)

    def test_ages_turn_unread_to_smoke_and_fire(self):
        self.send("a\n", "--to", "bob")
        ages = ("--smoke-after", "30m", "--fire-after", "3h")
        self.clock = at(10)
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(self.calls(self.tower_calls)[-1][2:4], ["green", "1 unread, 1 unheard, the oldest 10m"])
        self.clock = at(45)
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(self.calls(self.tower_calls)[-1][2:4], ["yellow", "1 unread, 1 unheard, the oldest 45m"])
        self.assertIn("mail", self.calls(self.tower_calls)[-1])  # tagged mail
        self.clock = at(200)
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(self.calls(self.tower_calls)[-1][2], "red")
        # Read without marking: heard, so clear, though still unread.
        self.dd("pull", "bob")
        self.clock = at(400)
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(self.calls(self.tower_calls)[-1][2:4], ["green", "1 unread"])
        # A copy kept by a mirror isn't hearing it: --unheard leaves no stamp.
        self.send("b\n", "--to", "bob")
        self.clock = at(500)
        self.dd("pull", "bob", "--unheard")
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(self.calls(self.tower_calls)[-1][2], "yellow")
        # Without ages, as before; and the ages must make sense.
        self.dd("firetower", "--box", "bob")
        self.assertEqual(self.calls(self.tower_calls)[-1][2:4], ["yellow", "2 unread"])
        self.dd("firetower", "--smoke-after", "3h", "--fire-after", "1h", ok=False)

    def rings(self):
        return [c for c in self.calls(self.cron_calls) if c and c[0] == "notify"]

    def test_ring_once_when_a_box_goes_to_fire(self):
        self.send("a secret plan\n", "--to", "bob")
        ages = ("--smoke-after", "30m", "--fire-after", "3h", "--ring")
        self.clock = at(10)
        self.dd("firetower", "--box", "bob", *ages)  # the first --ring run: notes, rings nothing
        self.assertEqual(self.rings(), [])
        self.clock = at(200)
        self.dd("firetower", "--box", "bob", "--box", "bob", *ages)  # named twice: one ring
        rings = self.rings()
        self.assertEqual(len(rings), 1)
        self.assertIn("deaddrop-box=bob", rings[0])
        self.assertEqual(rings[0][-1], "1 unread, 1 unheard, the oldest 3h")  # the board's line, no letter text
        # Still on fire: no second ring. Heard, then a new fire: one more.
        self.clock = at(210)
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(len(self.rings()), 1)
        self.dd("pull", "bob")
        self.dd("firetower", "--box", "bob", *ages)
        self.send("b\n", "--to", "bob")
        self.clock = at(500)
        self.dd("firetower", "--box", "bob", *ages)
        self.assertEqual(len(self.rings()), 2)

    def test_a_fire_already_burning_when_ring_starts_isnt_rung(self):
        self.send("a\n", "--to", "bob")
        self.clock = at(200)
        self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring")
        self.assertEqual(self.rings(), [])

    def test_a_ring_that_fails_isnt_counted_as_rung(self):
        self.send("a\n", "--to", "bob")
        self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring")  # first run: notes
        self.clock = at(200)
        broken = self.tmp / "broken-crontinental"
        broken.write_text("#!/bin/sh\necho no notify hooks >&2\nexit 1\n")
        broken.chmod(0o755)
        p = self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring", DEADDROP_CRONTINENTAL=str(broken), ok=False)
        self.assertIn("could not ring", p.stderr)
        # It works again: the fire that never rang rings now.
        self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring")
        self.assertEqual(len(self.rings()), 1)

    def test_an_old_urgent_letter_isnt_rung_again_with_ring_urgent(self):
        self.config.write_text("ring_urgent = true\n")
        self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring", ok=None)  # first run
        self.send("a\n", "--to", "bob", "--urgent")
        before = len(self.rings())
        self.clock = at(300)
        self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring")
        self.assertEqual(len(self.rings()), before)  # ring_urgent rang it as it came

    def test_a_broken_fire_list_is_said_not_rung_from(self):
        self.send("a\n", "--to", "bob")
        self.dd("firetower", "--box", "bob", "--ring")
        (self.tmp / "home" / ".local" / "state" / "deaddrop" / "firetower-fire.json").write_text("{not json")
        p = self.dd("firetower", "--box", "bob", "--ring", ok=False)
        self.assertIn("can't be read", p.stderr)

    def test_heard_means_shown(self):
        id = self.send("a\n", "--to", "bob", "--tag", "x")
        stamp = self.dir / "bob" / ".heard"
        self.dd("pull", "bob", "--tag", "y")  # filtered: shows only some of the box
        self.dd("show", id, "--box", "bob")  # one letter isn't the box
        self.dd("show", id, "--box", "bob", "--heard-at", at(2))  # an older mirror's refresh: taken, changes nothing
        self.dd("mark", "read", id, "--box", "bob")
        self.assertFalse(stamp.exists())
        self.dd("pull", "bob")  # the whole box: heard
        self.assertTrue(stamp.exists())
        self.dd("send", "--to", "bob,,carol", input="x\n", ok=False)  # a stray comma: said, not guessed

    def test_a_pigeonholes_fire_mark_outlives_a_run_without_pigeonholes(self):
        self.send("a\n", "--to", "bob", "--for", "planner")
        ph = ("--box", "bob", "--pigeonholes", "--fire-after", "3h", "--ring")
        self.dd("firetower", *ph)  # first run: notes
        self.clock = at(200)
        self.dd("firetower", *ph)
        self.assertEqual(len(self.rings()), 1)
        self.dd("firetower", "--box", "bob", "--fire-after", "3h", "--ring")  # another job, no --pigeonholes
        self.dd("firetower", *ph)
        self.assertEqual(len(self.rings()), 1)  # the planner's fire isn't rung again

    def test_an_urgent_ring_in_a_pigeonhole_says_so(self):
        self.config.write_text("ring_urgent = true\n")
        self.send("a\n", "--to", "bob", "--for", "planner", "--urgent")
        rings = self.rings()
        self.assertTrue(rings and "deaddrop-for=planner" in rings[-1], rings)

    def test_pigeonholes_on_the_board(self):
        self.send("a\n", "--to", "bob", "--for", "planner")
        self.dd("firetower", "--box", "bob")
        self.assertNotIn("mail/bob/for/planner", self.reports())  # only when asked
        ages = ("--box", "bob", "--pigeonholes", "--smoke-after", "30m", "--fire-after", "3h", "--ring")
        self.dd("firetower", *ages)  # first --ring run: notes
        self.clock = at(200)
        self.dd("firetower", *ages)
        r = self.calls(self.tower_calls)
        last = {c[1]: c for c in r}
        self.assertEqual(last["mail/bob/inbox"][2], "green")       # bob's own: nothing waiting
        self.assertEqual(last["mail/bob/for/planner"][2], "red")   # the planner's: waited 3h unheard
        rings = self.rings()
        self.assertEqual(len(rings), 1)
        self.assertIn("deaddrop-for=planner", rings[0])
        # The planner's pull hears its pigeonhole, not bob's.
        self.dd("pull", "bob", "--for", "planner")
        self.dd("firetower", *ages)
        self.assertEqual({c[1]: c for c in self.calls(self.tower_calls)}["mail/bob/for/planner"][2], "green")

    def test_nothing_private_on_the_board(self):
        self.send("secret plans\n", "--to", "bob", "--subject", "Acquisition of Example Corp", DEADDROP_FROM="carol")
        self.dd("firetower")
        report = " ".join(self.calls(self.tower_calls)[0])
        for word in ("Acquisition", "Example", "carol", "secret"):
            self.assertNotIn(word, report)

    def msg(self, box):
        return self.reports()["mail/%s/inbox" % box][3]

    def test_board_detail_is_off_unless_the_box_is_named(self):
        self.config.write_text('board_detail = ["alice"]\n')
        self.send("x\n", "--to", "bob", "--subject", "Acquisition of Example Corp", DEADDROP_FROM="carol")
        self.send("x\n", "--to", "dave", "--urgent", "--subject", "Private matter", DEADDROP_FROM="carol")
        self.dd("firetower")
        self.assertEqual(self.msg("bob"), "1 unread")
        self.assertEqual(self.msg("dave"), "1 urgent")
        report = " ".join(" ".join(c) for c in self.calls(self.tower_calls))
        for word in ("Acquisition", "Private", "carol"):
            self.assertNotIn(word, report)

    def test_board_detail_says_whats_waiting(self):
        self.config.write_text('board_detail = ["bob"]\n')
        self.send("x\n", "--to", "bob", "--subject", "Requests for tool X", DEADDROP_FROM="carol")
        self.send("x\n", "--to", "dave", "--subject", "Not on the board", DEADDROP_FROM="carol")
        self.dd("firetower", DEADDROP_NOW=at(5))
        self.assertEqual(self.msg("bob"), "1 unread · from carol, 5m ago: Requests for tool X")
        self.assertEqual(self.msg("dave"), "1 unread")
        c = self.reports()["mail/bob/inbox"]
        self.assertEqual(c[c.index("--label") + 1], "Mail: bob")
        self.assertRegex(c[c.index("--value") + 1], r"^[0-9a-f]{12}$")
        self.clock = at(10)
        self.send("x\n", "--to", "bob", "--subject", "Field notes", DEADDROP_FROM="erin")
        self.dd("firetower", DEADDROP_NOW=at(12))
        self.assertEqual(self.calls(self.tower_calls)[-2][3], "2 unread · newest from erin, 2m ago: Field notes")

    def test_board_detail_puts_urgent_first(self):
        self.config.write_text('board_detail = ["bob"]\n')
        self.send("x\n", "--to", "bob", "--urgent", "--subject", "The build is down", DEADDROP_FROM="carol")
        self.clock = at(60)
        self.send("x\n", "--to", "bob", "--subject", "Newer but not urgent", DEADDROP_FROM="erin")
        self.send("x\n", "--to", "bob", "--subject", "Also not urgent", DEADDROP_FROM="erin")
        self.dd("firetower", DEADDROP_NOW=at(120))
        self.assertEqual(self.msg("bob"), "1 urgent, 3 unread · urgent from carol, 2h ago: The build is down")

    def test_board_detail_cuts_a_long_subject(self):
        self.config.write_text('board_detail = ["bob"]\n')
        self.send("x\n", "--to", "bob", "--subject", "word " * 60, DEADDROP_FROM="carol")
        self.dd("firetower")
        m = self.msg("bob")
        self.assertLessEqual(len(m), 200)
        self.assertTrue(m.startswith("1 unread · from carol, just now: word word"))
        self.assertTrue(m.endswith("word…"), m)
        self.assertNotIn("\n", m)

    def test_board_detail_cleans_the_subject(self):
        # The CLI cleans subjects when it sends, but the file is people's to
        # read and write: the board line is cleaned again.
        self.config.write_text('board_detail = ["bob"]\n')
        sent = self.send("x\n", "--to", "bob", "--subject", "Plain", DEADDROP_FROM="carol")
        path = next((self.dir / "bob" / "new").glob("*.md"))
        text = path.read_text().replace('subject: "Plain"', 'subject: "Red\\u001b[31m alert\\tnow\\u0007\\r\\nsecond line"')
        path.write_text(text)
        self.dd("firetower")
        m = self.msg("bob")
        self.assertEqual(m, "1 unread · from carol, just now: Red[31m alert now second line")
        self.assertFalse(any(ord(ch) < 32 for ch in m))

    def test_next_from_crontinental(self):
        self.send("x\n", "--to", "bob")
        self.dd("firetower", CRONTINENTAL_NEXT="2026-11-02T15:35:00Z")
        c = self.calls(self.tower_calls)[0]
        self.assertEqual(c[c.index("--next") + 1], "2026-11-02T15:40:00Z")
        self.dd("firetower", "--next", "6m", CRONTINENTAL_NEXT="2026-11-02T15:35:00Z")
        c = self.calls(self.tower_calls)[1]
        self.assertEqual(c[c.index("--next") + 1], "6m")

    def test_one_box(self):
        self.send("x\n", "--to", "bob")
        self.send("x\n", "--to", "carol")
        self.dd("firetower", "--box", "carol")
        self.assertEqual(list(self.reports()), ["mail/carol/inbox"])

    def test_a_failing_firetower_is_an_error(self):
        self.send("x\n", "--to", "bob")
        broken = self.tmp / "broken-firetower"
        broken.write_text("#!/bin/sh\necho 'firetower: nope' >&2\nexit 1\n")
        broken.chmod(0o755)
        p = self.dd("firetower", DEADDROP_FIRETOWER=str(broken), ok=False)
        self.assertIn("nope", p.stderr)


class FollowUp(DeadDrop):
    def booking(self, n=0):
        c = self.calls(self.cron_calls)[n]
        return c, c[c.index("--") + 1:]

    def fire(self, n=0):
        """Runs the job the way Crontinental would when it comes due."""
        _, cmd = self.booking(n)
        env = self.env(DEADDROP_DIR=None)
        return subprocess.run(cmd, capture_output=True, text=True, env=env, check=True)

    def test_jobs_run_the_installed_deaddrop(self):
        # The job outlives this binary: an upgrade replaces a release's folder,
        # so it runs the installed one, whose path stays put.
        installed = self.tmp / "home" / ".local" / "bin" / "deaddrop"
        installed.parent.mkdir(parents=True)
        os.symlink(BIN, installed)
        self.send("x\n", "--to", "bob", "--follow-up", "1d")
        _, cmd = self.booking(0)
        self.assertEqual(cmd[-4], str(installed))
        # The installer's DEADDROP_BIN_DIR, when it was used.
        other = self.tmp / "bin-elsewhere"
        other.mkdir()
        os.symlink(BIN, other / "deaddrop")
        self.send("x\n", "--to", "bob", "--follow-up", "1d", DEADDROP_BIN_DIR=str(other))
        self.assertEqual(self.booking(1)[1][-4], str(other / "deaddrop"))
        # Nothing installed: whatever booked it, as a last resort.
        installed.unlink()
        self.send("x\n", "--to", "bob", "--follow-up", "1d", PATH="/usr/bin:/bin")
        self.assertEqual(os.path.realpath(self.booking(2)[1][-4]), os.path.realpath(BIN))

    def test_books_a_one_off_job(self):
        id = self.send("x\n", "--to", "bob", "--follow-up", "2d")
        args, cmd = self.booking()
        self.assertEqual(args[:2], ["in", str(2 * 24 * 3600) + "s"])
        name = args[args.index("--name") + 1]
        self.assertRegex(name, r"^deaddrop-2026-11-02t153000-[0-9a-f]{4}-[0-9a-f]{4}$")
        self.assertLessEqual(len(name), 49)  # Crontinental's limit
        self.assertEqual(args[args.index("--notify") + 1], "never")
        self.assertEqual(cmd[-3:], ["follow-up", "bob", id])
        self.assertIn("DEADDROP_DIR=" + str(self.dir), cmd)
        self.me = "bob"
        self.assertEqual(self.inbox()[0]["follow_up"], "2026-11-04T15:30:00Z")

    def test_an_unread_message_turns_urgent(self):
        # Build order step 5.
        id = self.send("x\n", "--to", "bob", "--follow-up", "1m")
        self.clock = at(1)
        out = self.fire().stdout
        self.assertIn("raised to urgent", out)
        self.me = "bob"
        m = self.inbox()[0]
        self.assertEqual((m["id"], m["urgency"], m["raised"]), (id, "urgent", "2026-11-02T15:31:00Z"))
        self.assertEqual(m["body"] if "body" in m else "x\n", "x\n")
        self.assertEqual(json.loads(self.dd("pull", "--json").stdout)[0]["body"], "x\n")

    def test_a_read_message_doesnt(self):
        id = self.send("x\n", "--to", "bob", "--follow-up", "1m")
        self.me = "bob"
        self.dd("mark", "read", id)
        # Reading it cancels the job by name...
        rm = self.calls(self.cron_calls)[1]
        self.assertEqual(rm[:2], ["rm", self.booking()[0][self.booking()[0].index("--name") + 1]])
        self.assertIn("--force", rm)
        # ...and if it runs anyway, it does nothing.
        self.clock = at(1)
        self.assertIn("nothing to do", self.fire().stdout)
        m = self.inbox("--all")[0]
        self.assertEqual((m["state"], m["urgency"]), ("read", "normal"))

    def test_follow_up_at_a_time(self):
        self.send("x\n", "--to", "bob", "--follow-up", "2026-11-02T16:00:00Z")
        self.assertEqual(self.booking()[0][:2], ["in", "1800s"])
        p = self.dd("send", "--to", "bob", "--follow-up", "2026-11-01T09:00:00Z", input="x\n", ok=False)
        self.assertIn("already past", p.stderr)

    def test_no_crontinental_no_send(self):
        p = self.dd("send", "--to", "bob", "--follow-up", "1d", input="x\n", ok=False,
                    DEADDROP_CRONTINENTAL=None, PATH="/usr/bin:/bin")
        self.assertIn("crontinental", p.stderr)
        self.assertFalse((self.dir / "bob").exists())

    def test_a_failed_booking_still_delivers_once(self):
        broken = self.tmp / "broken-cron"
        broken.write_text("#!/bin/sh\necho 'bad job name' >&2\nexit 2\n")
        broken.chmod(0o755)
        p = self.dd("send", "--to", "bob", "--follow-up", "1d", input="x\n", DEADDROP_CRONTINENTAL=str(broken))
        self.assertIn("isn't booked", p.stderr)
        self.assertEqual(len(self.files("bob", "new")), 1)


class Ring(DeadDrop):
    def test_off_by_default(self):
        self.send("x\n", "--to", "bob", "--urgent")
        self.assertEqual(self.calls(self.cron_calls), [])

    def test_urgent_mail_rings_without_saying_what(self):
        self.config.write_text("ring_urgent = true\n")
        self.send("x\n", "--to", "bob", "--subject", "Private matter", "--urgent")
        self.send("x\n", "--to", "bob")
        calls = self.calls(self.cron_calls)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:2], ["notify", "--urgent"])
        self.assertNotIn("Private", " ".join(calls[0]))
        self.assertIn("bob", calls[0][-1])

    def test_board_detail_rings_with_what(self):
        self.config.write_text('ring_urgent = true\nboard_detail = ["bob"]\n')
        self.send("x\n", "--to", "bob", "--subject", "The build is down", "--urgent", DEADDROP_FROM="carol")
        self.send("x\n", "--to", "dave", "--subject", "Private matter", "--urgent", DEADDROP_FROM="carol")
        calls = self.calls(self.cron_calls)
        self.assertEqual(calls[0][-1], "bob: urgent from carol, just now: The build is down")
        self.assertIn("drop=" + self.ids(DEADDROP_FROM="bob")[0], calls[0])
        self.assertEqual(calls[1][-1], "dave: urgent mail waiting")

    def test_a_broken_board_detail_is_refused(self):
        self.config.write_text('board_detail = ["Not A Box"]\n')
        p = self.dd("status", ok=False)
        self.assertIn("board_detail", p.stderr)

    def test_rings_are_capped_per_sender_and_box(self):
        # So no one sender (a gated key's above all) can page without end;
        # the letter is still urgent and on the board, only the ring is held.
        self.config.write_text("ring_urgent = true\n")
        for i in range(4):
            self.send("x\n", "--to", "bob", "--urgent", DEADDROP_NOW=at(i))
        p = self.dd("send", "--to", "bob", "--urgent", input="x\n", DEADDROP_NOW=at(5))
        self.assertIn("not rung: alice has rung bob 4 times in the last hour", p.stderr)
        self.assertEqual(len(self.calls(self.cron_calls)), 4)
        self.assertIn("urgent", self.dd("inbox", "bob", DEADDROP_NOW=at(5)).stdout)
        self.dd("send", "--to", "bob", "--urgent", input="x\n", DEADDROP_NOW=at(5))
        self.assertEqual((self.dir / "gate.log").read_text().count("rings held"), 1)  # one line an hour, not one a letter
        # Another box, or another sender, still rings; so does alice, an hour on.
        self.send("x\n", "--to", "carol", "--urgent", DEADDROP_NOW=at(6))
        self.send("x\n", "--to", "bob", "--urgent", DEADDROP_NOW=at(6), DEADDROP_FROM="dave")
        self.send("x\n", "--to", "bob", "--urgent", DEADDROP_NOW=at(61))
        self.assertEqual(len(self.calls(self.cron_calls)), 7)

    def test_one_urgent_letter_to_a_whole_send_rings_everyone(self):
        self.config.write_text("ring_urgent = true\n")
        to = [w for i in range(32) for w in ("--to", f"box{i}")]
        self.send("x\n", *to, "--urgent", DEADDROP_NOW=at(0))
        self.assertEqual(len(self.calls(self.cron_calls)), 32)
        p = self.dd("send", "--to", "late", "--urgent", input="x\n", DEADDROP_NOW=at(1))
        self.assertIn("alice has rung 32 times in the last hour", p.stderr)
        self.assertEqual(len(self.calls(self.cron_calls)), 32)

    def test_a_ring_that_fails_doesnt_spend_the_cap(self):
        self.config.write_text("ring_urgent = true\n")
        broken = script(self.tmp / "broken-cron", "exit 1\n")
        for i in range(5):
            self.dd("send", "--to", "bob", "--urgent", input="x\n", DEADDROP_NOW=at(i), DEADDROP_CRONTINENTAL=str(broken))
        self.dd("send", "--to", "bob", "--urgent", input="x\n", DEADDROP_NOW=at(6),
                DEADDROP_CRONTINENTAL=str(self.tmp / "no-such-cron"))
        self.send("x\n", "--to", "bob", "--urgent", DEADDROP_NOW=at(7))
        self.assertEqual(len(self.calls(self.cron_calls)), 1)  # rung: nothing before it went out

    def test_automation_isnt_capped(self):
        # Only the store login's own jobs send as automation; one job's
        # urgent mail mustn't hold another's ring.
        self.config.write_text("ring_urgent = true\n")
        for i in range(6):
            self.send("x\n", "--to", "bob", "--urgent", "--from", "automation", DEADDROP_NOW=at(i))
        self.assertEqual(len(self.calls(self.cron_calls)), 6)

    def test_a_raised_follow_up_rings(self):
        self.config.write_text("ring_urgent = true\n")
        self.send("x\n", "--to", "bob", "--follow-up", "1m")
        c = self.calls(self.cron_calls)[0]
        cmd = c[c.index("--") + 1:]
        subprocess.run(cmd, check=True, capture_output=True, env=self.env(DEADDROP_DIR=None, DEADDROP_NOW=at(1)))
        self.assertEqual(self.calls(self.cron_calls)[1][:2], ["notify", "--urgent"])
