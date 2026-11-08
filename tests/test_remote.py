"""Remote mode: every command runs on the store's machine over ssh, as the
caller, and nothing is written on the caller's side.

The far machine is a second HOME on this one, reached through a stand-in for
ssh that runs the command line it is given there, with a clean environment,
the way sshd would."""

import json
import os
import subprocess
import time
from pathlib import Path

from _binary import BIN, DeadDrop, at, key, script, signers


class RemoteBase(DeadDrop):
    def setUp(self):
        super().setUp()
        self.far = self.tmp / "far-home"
        (self.far / ".local" / "bin").mkdir(parents=True)
        os.symlink(BIN, self.far / ".local" / "bin" / "deaddrop")
        self.far_store = self.far / ".local" / "share" / "deaddrop"
        self.near = self.tmp / "near-home"
        self.near.mkdir()
        self.ssh_log = self.tmp / "ssh.log"
        # Everything after the host is the command line; the stand-in runs the
        # last argument under sh with the far machine's HOME, and nothing of
        # the caller's environment.
        self.ssh = script(self.tmp / "ssh", f'''
printf '%s\\n' "$@" >> "{self.ssh_log}"
for last; do :; done
exec env -i PATH=/usr/bin:/bin HOME="{self.far}" DEADDROP_NOW="$DEADDROP_NOW" \\
  DEADDROP_CRONTINENTAL="{self.cron}" sh -c "$last"
''')

    def near_env(self, **extra):
        e = dict(HOME=str(self.near), DEADDROP_HOST="store-box", DEADDROP_SSH=str(self.ssh),
                 DEADDROP_DIR=None, DEADDROP_CONFIG=str(self.near / "config.toml"))
        e.update(extra)
        return e

    def rdd(self, *args, **kw):
        return self.dd(*args, **self.near_env(**kw.pop("env", {})), **kw)

    def near_files(self):
        """What reading left on this side: nothing but the reader's own
        ledger of seals (hashes and ids, never a letter's text)."""
        ledger = {".local", ".local/state", ".local/state/deaddrop", ".local/state/deaddrop/seals.json", ".local/state/deaddrop/.seals.lock",
                  ".local/state/deaddrop/verified.json", ".local/state/deaddrop/.verified.lock"}
        return sorted(str(p.relative_to(self.near)) for p in self.near.rglob("*") if str(p.relative_to(self.near)) not in ledger)


class Remote(RemoteBase):
    def test_send_pull_mark_with_nothing_written_here(self):
        # Build order step 3.
        att = self.tmp / "outside" / "run.log"
        att.parent.mkdir()
        att.write_text("run 1: 3 issues\n")
        p = self.rdd("send", "--to", "bob", "--tag", "tool-y", "--attach", str(att), input="# Field notes\n\nGood run.\n")
        id = p.stdout.split()[1]
        self.assertRegex(id, r"-alice-")
        self.assertTrue((self.far_store / "bob" / "new" / (id + ".md")).exists())
        self.assertEqual((self.far_store / "bob" / "files" / id / "run.log").read_text(), "run 1: 3 issues\n")

        bob = dict(DEADDROP_FROM="bob")
        pulled = json.loads(self.rdd("pull", "--tag", "tool-y", "--json", env=bob).stdout)
        self.assertEqual([m["id"] for m in pulled], [id])
        self.assertEqual(pulled[0]["body"], "# Field notes\n\nGood run.\n")
        self.assertIn("Good run.", self.rdd("pull", env=bob).stdout)
        self.assertEqual(self.rdd("file", id, "run.log", env=bob).stdout, "run 1: 3 issues\n")
        self.rdd("mark", "read", id, env=bob)
        self.assertTrue((self.far_store / "bob" / "read" / (id + ".md")).exists())
        self.rdd("reply", id, input="Thanks.\n", env=bob)
        self.assertEqual(len(json.loads(self.rdd("inbox", "--json").stdout)), 1)
        self.assertIn("bob", self.rdd("status").stdout)

        self.assertEqual(self.near_files(), [], "remote mode wrote on the caller's side")

    def test_identity_goes_across_and_is_required(self):
        p = self.rdd("send", "--to", "bob", input="x\n", env=dict(DEADDROP_FROM=None, USER="zed"), ok=False)
        self.assertIn("say who you are", p.stderr)
        self.assertFalse(self.far_store.exists())
        # me in the near config does.
        (self.near / "config.toml").write_text('me = "carol"\nhost = "store-box"\n')
        id = self.rdd("send", "--to", "me", input="x\n", env=dict(DEADDROP_FROM=None, DEADDROP_HOST=None)).stdout.split()[1]
        self.assertTrue((self.far_store / "carol" / "new" / (id + ".md")).exists())

    def test_arguments_survive_the_shell(self):
        subject = "it's $HOME; `touch /tmp/x` \"quoted\" & more"
        self.rdd("send", "--to", "bob", "--subject", subject, input="x\n")
        m = json.loads(self.rdd("inbox", "bob", "--json").stdout)[0]
        self.assertEqual(m["subject"], subject)

    def test_failures_come_back(self):
        self.rdd("send", "--to", "carol", input="x\n")
        p = self.rdd("mark", "read", "2026-11-02T153000-bob-0000", ok=False)
        self.assertIn("no box alice", p.stderr)
        self.assertEqual(p.returncode, 4)  # no such box: its own code, across ssh
        unreachable = script(self.tmp / "ssh-down", "echo 'ssh: connect to host store-box: Connection refused' >&2; exit 255\n")
        for cmd in (("inbox",), ("ls",), ("mark", "read", "--tag", "x"), ("sent",)):
            self.assertEqual(self.rdd(*cmd, env=dict(DEADDROP_SSH=str(unreachable)), ok=False).returncode, 255, cmd)
        p = self.rdd("status", env=dict(DEADDROP_SSH=str(unreachable)), ok=False)
        self.assertIn("could not reach the store on store-box", p.stderr)

    def test_ssh_is_batch_and_no_stdin_unless_needed(self):
        self.rdd("send", "--to", "carol", input="x\n")
        self.ssh_log.unlink()
        self.rdd("status")
        args = self.ssh_log.read_text().splitlines()
        self.assertIn("BatchMode=yes", args)
        self.assertIn("-n", args)

    def test_remote_bin(self):
        (self.near / "config.toml").write_text(f'host = "store-box"\nremote_bin = "{BIN}"\n')
        os.remove(self.far / ".local" / "bin" / "deaddrop")
        self.rdd("send", "--to", "bob", input="x\n", env=dict(DEADDROP_HOST=None))
        self.assertEqual(len(list((self.far_store / "bob" / "new").glob("*.md"))), 1)

    def test_a_machine_that_isnt_the_store_says_so(self):
        # A command that arrives over ssh never goes round again on its own:
        # a machine whose config points elsewhere, without relay, refuses
        # rather than answering from an empty store of its own.
        (self.far / ".config" / "deaddrop").mkdir(parents=True)
        (self.far / ".config" / "deaddrop" / "config.toml").write_text('host = "elsewhere"\n')
        for cmd in (["send", "--to", "bob"], ["inbox"], ["mark", "read", "--tag", "x"], ["watch"]):
            p = self.rdd(*cmd, input="x\n", ok=False, env=dict(DEADDROP_STATE=str(self.tmp / "st")))
            self.assertIn("set relay = true", p.stderr, cmd)
        self.assertFalse(self.far_store.exists())

    def test_no_store_there_is_an_error_not_an_empty_box(self):
        for cmd in (["inbox"], ["mark", "read", "--tag", "x"], ["show", "2026-11-02T153000-bob-ffff"], ["status"]):
            p = self.rdd(*cmd, ok=False)
            self.assertIn("no Dead Drop store on this machine", p.stderr, cmd)
        p = self.rdd("watch", ok=False, env=dict(DEADDROP_STATE=str(self.tmp / "st")))
        self.assertIn("no Dead Drop store", p.stderr)
        self.assertFalse(self.far_store.exists())
        # The first send starts it.
        self.rdd("send", "--to", "bob", input="x\n")
        self.assertEqual(self.rdd("inbox", env=dict(DEADDROP_FROM="bob")).returncode, 0)

    def test_watch_over_ssh_and_an_unreachable_store(self):
        state = self.tmp / "near-state"
        env = dict(DEADDROP_STATE=str(state))
        self.rdd("send", "--to", "alice", input="# Hello\n\nfar text\n", env=dict(DEADDROP_FROM="bob", **env))
        self.rdd("watch", env=env)
        f = json.loads((state / "inbox.json").read_text())
        self.assertTrue(f["ok"])
        self.assertEqual([m["subject"] for m in f["messages"]], ["Hello"])
        # Only the state folder is written on this side, and no text in it.
        self.assertEqual(self.near_files(), [])
        self.assertNotIn("far text", (state / "inbox.json").read_text())

        down = script(self.tmp / "down-ssh", "echo 'ssh: connect to host store-box: Network is unreachable' >&2; exit 255\n")
        p = self.rdd("watch", ok=False, env=dict(DEADDROP_SSH=str(down), **env))
        self.assertIn("could not reach the store", p.stderr)
        f = json.loads((state / "inbox.json").read_text())
        self.assertFalse(f["ok"])
        self.assertIn("could not reach", f["error"])
        self.assertEqual([m["subject"] for m in f["messages"]], ["Hello"])  # the last list, kept
        self.assertIn("can't reach the store", self.rdd("bar", env=env).stdout)

        self.rdd("watch", env=env)
        f = json.loads((state / "inbox.json").read_text())
        self.assertTrue(f["ok"])
        self.assertNotIn("error", f)

    def relay_setup(self, host="store-box"):
        """A machine between: this one reaches it, and it reaches the store."""
        mid = self.tmp / "mid-home"
        (mid / ".local" / "bin").mkdir(parents=True)
        os.symlink(BIN, mid / ".local" / "bin" / "deaddrop")
        (mid / ".config" / "deaddrop").mkdir(parents=True)
        (mid / ".config" / "deaddrop" / "config.toml").write_text(f'host = "{host}"\nrelay = true\n')
        self.mid_log = self.tmp / "mid-ssh.log"
        # The middle's own ssh is the stand-in that reaches the store.
        mid_ssh = script(self.tmp / "mid-ssh", f'''
printf '%s\\n' "$@" >> "{self.mid_log}"
for last; do :; done
exec env -i PATH=/usr/bin:/bin HOME="{mid}" DEADDROP_NOW="$DEADDROP_NOW" DEADDROP_SSH="{self.ssh}" sh -c "$last"
''')
        return mid, mid_ssh

    def test_through_a_relay(self):
        mid, mid_ssh = self.relay_setup()
        env = dict(DEADDROP_SSH=str(mid_ssh), DEADDROP_HOST="relay-box")
        att = self.tmp / "outside" / "run.log"
        att.parent.mkdir()
        att.write_text("two hops\n")
        id = self.rdd("send", "--to", "bob", "--attach", str(att), input="# Far\n\nx\n", env=env).stdout.split()[1]
        self.assertRegex(id, r"-alice-")
        self.assertEqual((self.far_store / "bob" / "files" / id / "run.log").read_text(), "two hops\n")
        # Nothing landed on the relay but its config.
        self.assertFalse((mid / ".local" / "share" / "deaddrop").exists())
        bob = dict(DEADDROP_FROM="bob", **env)
        self.assertEqual([m["id"] for m in json.loads(self.rdd("inbox", "--json", env=bob).stdout)], [id])
        self.rdd("mark", "read", id, env=bob)
        self.assertTrue((self.far_store / "bob" / "read" / (id + ".md")).exists())
        state = self.tmp / "near-state"
        self.rdd("watch", env=dict(DEADDROP_STATE=str(state), **bob))
        f = json.loads((state / "inbox.json").read_text())
        self.assertEqual(([m["id"] for m in f["messages"]], f["box"]), ([id], "bob"))

    def test_a_relay_loop_ends(self):
        mid, mid_ssh = self.relay_setup()
        # The relay's host is the relay itself.
        self.ssh.write_text(self.ssh.read_text().replace(str(self.far), str(mid)).replace(
            "sh -c", f'DEADDROP_SSH="{self.ssh}" sh -c'))
        p = self.rdd("status", ok=False, env=dict(DEADDROP_SSH=str(mid_ssh), DEADDROP_HOST="relay-box"))
        self.assertIn("come through 3 machines", p.stderr)

    # ---- signatures across machines ----

    def signed_env(self, who, **extra):
        e = dict(DEADDROP_FROM=who, DEADDROP_SIGN_KEY=key(who)[0],
                 DEADDROP_SIGNERS=str(signers(self.tmp / "near_signers", "alice", "bob")))
        e.update(extra)
        return e

    def test_a_stores_signed_key_is_never_taken(self):
        # A hostile or older store puts a fingerprint on an unsigned letter;
        # the reader works signed_key out itself, only for a verified one.
        self.rdd("send", "--to", "bob", input="# Unsigned\n\nx\n")
        self.ssh = script(self.tmp / "ssh-forging", f'''
for last; do :; done
env -i PATH=/usr/bin:/bin HOME="{self.far}" DEADDROP_NOW="$DEADDROP_NOW" sh -c "$last" | sed 's/"from":/"signed_key":"SHA256:forged","from":/g'
''')
        ms = json.loads(self.rdd("inbox", "--json", env=dict(DEADDROP_FROM="bob")).stdout)
        self.assertEqual(ms[0].get("signature"), "unsigned")
        self.assertNotIn("signed_key", ms[0])

    def test_signed_here_checked_here(self):
        att = self.tmp / "outside" / "run.log"
        att.parent.mkdir()
        att.write_text("run 41\n")
        id = self.rdd("send", "--to", "bob", "--attach", str(att), input="# Notes\n\nx\n", env=self.signed_env("alice")).stdout.split()[1]
        bob = self.signed_env("bob")
        ms = json.loads(self.rdd("inbox", "--json", env=bob).stdout)
        self.assertEqual((ms[0]["signature"], ms[0]["signed_by"]), ("verified", "alice"))
        # The key itself, as ssh-keygen -l names it: a reader can pin one key.
        fp = subprocess.run(["ssh-keygen", "-lf", key("alice")[0] + ".pub"], capture_output=True, text=True).stdout.split()[1]
        self.assertEqual(ms[0]["signed_key"], fp)
        self.assertNotIn("sig", ms[0])
        self.assertNotIn("body", ms[0])
        self.assertIn("[signature: verified, signed by alice, ", self.rdd("pull", env=bob).stdout)
        self.assertIn("run.log (7 B)", self.rdd("pull", env=bob).stdout)
        self.assertEqual(json.loads(self.rdd("show", id, "--json", env=bob).stdout)["signature"], "verified")
        p = self.rdd("file", id, "run.log", env=bob)
        self.assertEqual((p.stdout, p.stderr), ("run 41\n", ""))
        # Changed on the store: caught here.
        f = next((self.far_store / "bob" / "new").glob("*.md"))
        f.write_text(f.read_text().replace("x\n", "y\n"))
        self.assertEqual(json.loads(self.rdd("show", id, "--json", env=bob).stdout)["signature"], "bad")
        (self.far_store / "bob" / "files" / id / "run.log").write_text("run 99\n")
        self.assertIn("isn't the file that was sent", self.rdd("file", id, "run.log", ok=False, env=bob).stderr)
        # Nothing written here.
        self.assertEqual(self.near_files(), [])

    def test_the_stores_own_list_never_counts(self):
        # Mallory signs as alice, and the store's list says that key is
        # alice's. The reader's list doesn't, and only the reader's counts.
        far_cfg = self.far / ".config" / "deaddrop"
        far_cfg.mkdir(parents=True)
        signers(far_cfg / "allowed_signers", alice="mallory")
        self.rdd("send", "--to", "bob", input="# Pay mallory\n\nx\n",
                 env=dict(DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=key("mallory")[0]))
        m = json.loads(self.rdd("inbox", "--json", env=self.signed_env("bob")).stdout)[0]
        self.assertEqual(m["signature"], "bad")
        self.assertIn("isn't alice's key", m["signature_note"])

    def test_a_reply_over_ssh_is_signed(self):
        id = self.rdd("send", "--to", "bob", "--tag", "tool-x", input="# Q\n\nx\n", env=self.signed_env("alice")).stdout.split()[1]
        self.rdd("reply", id, input="Yes.\n", env=self.signed_env("bob"))
        m = json.loads(self.rdd("inbox", "--json", env=self.signed_env("alice")).stdout)[0]
        self.assertEqual((m["subject"], m["thread"], m["signature"], m["signed_by"]), ("Re: Q", id, "verified", "bob"))

    def test_signatures_pass_through_a_relay(self):
        mid, mid_ssh = self.relay_setup()
        via = dict(DEADDROP_SSH=str(mid_ssh), DEADDROP_HOST="relay-box")
        att = self.tmp / "outside" / "a.txt"
        att.parent.mkdir()
        att.write_text("a\n")
        self.rdd("send", "--to", "bob", "--attach", str(att), input="# Far\n\nx\n", env=self.signed_env("alice", **via))
        m = json.loads(self.rdd("inbox", "--json", env=self.signed_env("bob", **via)).stdout)[0]
        self.assertEqual((m["signature"], m["signed_by"]), ("verified", "alice"))

    def test_an_older_store_says_to_upgrade_it(self):
        # What a deaddrop from before 0.3 says to flags it doesn't know.
        old = script(self.tmp / "old-deaddrop", """
for a; do case "$a" in --signed-by|--signed-by=*|--raw) echo "deaddrop: unknown flag ${a%%=*}" >&2; echo "usage: deaddrop ..." >&2; exit 2;; esac; done
exit 0
""")
        env = dict(DEADDROP_REMOTE_BIN=str(old))
        p = self.rdd("send", "--to", "bob", input="# Hi\n\nx\n", ok=False, env=self.signed_env("alice", **env))
        self.assertIn("from before 0.3", p.stderr)
        self.assertNotIn("unknown flag", p.stderr)
        p = self.rdd("inbox", ok=False, env=self.signed_env("bob", **env))
        self.assertIn("from before 0.3", p.stderr)


    def test_sent_from_a_store_without_wrapped(self):
        # A 0.7.6 store doesn't know --wrapped: asked again without it.
        old = script(self.tmp / "store-076", f"""
for a; do [ "$a" = --wrapped ] && {{ echo "deaddrop: unknown flag --wrapped" >&2; exit 2; }}; done
exec "{BIN}" "$@"
""")
        self.rdd("send", "--to", "bob", input="# To bob\n\nx\n", env=self.signed_env("alice"))
        rows = json.loads(self.rdd("sent", "--json", env=dict(DEADDROP_FROM="alice", DEADDROP_REMOTE_BIN=str(old))).stdout)
        self.assertEqual([r["subject"] for r in rows], ["To bob"])

    def test_sent_over_ssh_is_checked_here(self):
        self.rdd("send", "--to", "bob,carol", "--as", "Helper", input="# Both\n\nx\n", env=self.signed_env("alice"))
        self.rdd("send", "--to", "alice", input="# Not mine\n\nx\n", env=self.signed_env("bob"))
        ss = json.loads(self.rdd("sent", "--json", env=self.signed_env("alice")).stdout)
        self.assertEqual([s["subject"] for s in ss], ["Both"])
        s = ss[0]
        self.assertEqual((s["signature"], s["persona"], s["recipients"]), ("verified", "Helper", ["bob", "carol"]))
        self.assertEqual([c["box"] for c in s["copies"]], ["bob", "carol"])
        self.assertNotIn("sig", s)
        # Changed on the store: caught here.
        f = next((self.far_store / "carol" / "new").glob("*.md"))
        f.write_text(f.read_text().replace("x\n", "y\n"))
        f2 = next((self.far_store / "bob" / "new").glob("*.md"))
        f2.write_text(f2.read_text().replace("x\n", "y\n"))
        self.assertEqual(json.loads(self.rdd("sent", "--json", env=self.signed_env("alice")).stdout)[0]["signature"], "bad")
        self.assertEqual(self.near_files(), [])

    def test_sent_through_a_relay(self):
        mid, mid_ssh = self.relay_setup()
        via = dict(DEADDROP_SSH=str(mid_ssh), DEADDROP_HOST="relay-box")
        self.rdd("send", "--to", "bob", input="# Far\n\nx\n", env=self.signed_env("alice", **via))
        ss = json.loads(self.rdd("sent", "--json", env=self.signed_env("alice", **via)).stdout)
        self.assertEqual((ss[0]["subject"], ss[0]["signature"]), ("Far", "verified"))

    def test_a_thread_over_ssh_and_from_an_older_store(self):
        a = self.rdd("send", "--to", "bob", input="# Q\n\nx\n", env=self.signed_env("alice")).stdout.split()[1]
        self.rdd("reply", a, input="Yes.\n", env=self.signed_env("bob"))
        c = json.loads(self.rdd("with", "bob", "--json", env=self.signed_env("alice")).stdout)
        self.assertEqual([(l["mine"], l["signature"]) for l in c["letters"]], [(True, "verified"), (False, "verified")])
        self.assertEqual(self.near_files(), [])
        # A store from before 0.4 has no sent: their side only, and it says so.
        old = script(self.tmp / "old-deaddrop", f"""
[ "$1" = sent ] && {{ echo 'deaddrop: no command "sent" (deaddrop help lists them)' >&2; exit 2; }}
for a; do [ "$a" = --all ] && [ "$1" = pull ] && {{ echo 'deaddrop: unknown flag --all' >&2; exit 2; }}; done
exec {BIN} "$@"
""")
        c = json.loads(self.rdd("with", "bob", "--json", env=self.signed_env("alice", DEADDROP_REMOTE_BIN=str(old))).stdout)
        self.assertEqual([l["mine"] for l in c["letters"]], [False])
        self.assertIn("before 0.4", c["sent_missing"])
        self.assertIn("your side isn't shown", c["sent_missing"])
        self.assertIn("archived", c["sent_missing"])

    def test_helpers_over_ssh(self):
        self.rdd("send", "--to", "bob", "--for", "planner", input="# Lock\n\nx\n", env=self.signed_env("alice"))
        bob = self.signed_env("bob")
        self.assertEqual(json.loads(self.rdd("inbox", "--json", env=bob).stdout), [])
        ms = json.loads(self.rdd("inbox", "--json", env=dict(DEADDROP_FOR="planner", **bob)).stdout)
        self.assertEqual([(m["subject"], m["signature"], m["pigeonhole"]) for m in ms], [("Lock", "verified", "planner")])
        t = json.loads(self.rdd("threads", "--json", env=self.signed_env("alice")).stdout)
        self.assertEqual(t["topics"][0]["participants"][0]["for"], "planner")

    def test_a_reply_joins_the_thread_in_every_box_over_ssh(self):
        self.rdd("send", "--to", "bob,carol", input="# Council\n\nx\n", env=self.signed_env("alice"))
        bobs = json.loads(self.rdd("inbox", "--json", env=self.signed_env("bob")).stdout)[0]["id"]
        self.rdd("send", "--to", "alice,carol", "--reply-to", bobs, input="# Yes\n\nx\n", env=self.signed_env("bob"))
        t = json.loads(self.rdd("threads", "--json", env=dict(DEADDROP_FROM="carol")).stdout)["topics"]
        self.assertEqual([(x["subject"], x["count"]) for x in t], [("Council", 2)])

    def test_the_helper_hint_over_ssh(self):
        self.rdd("send", "--to", "bob", "--for", "planner", input="# For the planner\n\nx\n")
        out = self.rdd("inbox", env=dict(DEADDROP_FROM="bob")).stdout
        self.assertIn("unread for helpers: planner 1", out)


class RemoteDashes(RemoteBase):
    def test_reads_after_a_double_dash_still_reach_the_store_as_reads(self):
        # The flags remote mode adds go before a person's "--", not after.
        a = self.rdd("send", "--to", "bob", input="# A\n\nx\n").stdout.split()[1]
        for cmd in ("inbox", "pull"):
            p = self.rdd(cmd, "--", "bob", env=dict(DEADDROP_FROM="bob"))
            self.assertIn(a[-4:], p.stdout, (cmd, p.stderr))


    def test_a_box_nothing_was_sent_to_says_so_by_its_code_over_ssh(self):
        self.rdd("send", "--to", "bob", input="# A\n\nx\n")
        for args in (("inbox", "carol", "--json"), ("mark", "read", "--box", "carol", "--tag", "x")):
            p = self.rdd(*args, ok=False, env=dict(DEADDROP_FROM="bob"))
            self.assertEqual(p.returncode, 4, (args, p.stderr))
            self.assertIn("no box carol", p.stderr)


class RemoteWait(RemoteBase):
    def test_wait_over_ssh_reports_and_keeps_its_state_here(self):
        a = self.rdd("send", "--to", "bob", input="# A\n\nthe body\n").stdout.split()[1]
        p = self.rdd("wait", env=dict(DEADDROP_FROM="bob"), timeout=10)
        self.assertIn(a, p.stdout)
        self.assertNotIn("the body", p.stdout)
        self.assertTrue((self.near / ".local" / "state" / "deaddrop" / "waited@bob.json").exists())
        b = self.rdd("send", "--to", "bob", input="# B\n\nx\n").stdout.split()[1]
        p = self.rdd("wait", env=dict(DEADDROP_FROM="bob"), timeout=10)
        self.assertIn(b, p.stdout)
        self.assertNotIn(a, p.stdout)
        p = self.rdd("wait", "--box", "carol", ok=False, env=dict(DEADDROP_FROM="bob"), timeout=10)
        self.assertEqual(p.returncode, 4)


    def test_a_store_it_cant_reach_is_waited_out_and_alive_goes_stale(self):
        down = script(self.tmp / "ssh-down", "echo 'ssh: connect to host store-box: Connection refused' >&2; exit 255\n")
        alive = self.tmp / "alive"
        p = subprocess.Popen([BIN, "wait", "--alive", str(alive)], env=self.env(**{k: v for k, v in self.near_env(DEADDROP_FROM="bob", DEADDROP_SSH=str(down)).items()}),
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(1.5)
            self.assertIsNone(p.poll())  # still waiting
        finally:
            p.kill()
            out, err = p.communicate()
        self.assertIn("could not reach the store", err)
        self.assertEqual(err.count("still waiting"), 1)  # said once
        self.assertFalse(alive.exists())

    def test_an_older_stores_refusal_still_ends_the_wait(self):
        # A 0.7.7 store's gate refused with exit 1: waiting won't change it.
        old = script(self.tmp / "store-077", 'echo "deaddrop: this key reads only bob, not carol" >&2; exit 1\n')
        p = self.rdd("wait", "--box", "carol", ok=False, env=dict(DEADDROP_FROM="bob", DEADDROP_REMOTE_BIN=str(old)), timeout=10)
        self.assertEqual(p.returncode, 1)
        self.assertIn("reads only bob", p.stderr)
        self.assertNotIn("still waiting", p.stderr)

    def test_no_name_to_ask_as_ends_the_wait(self):
        p = self.rdd("wait", ok=False, env=dict(DEADDROP_FROM=None), timeout=10)
        self.assertIn("DEADDROP_FROM", p.stderr)
        self.assertEqual(p.returncode, 5)

    def test_other_commands_keep_exit_1_for_a_store_they_cant_reach_through_self(self):
        down = script(self.tmp / "ssh-down2", "echo 'ssh: connect to host store-box: Connection refused' >&2; exit 255\n")
        p = self.rdd("watch", ok=False, env=dict(DEADDROP_FROM="bob", DEADDROP_SSH=str(down)))
        self.assertEqual(p.returncode, 1)


class RemoteCaps(RemoteBase):
    def test_too_many_attachments_are_refused_before_connecting(self):
        self.rdd("send", "--to", "bob", input="# First\n\nx\n")
        files = []
        for i in range(17):
            f = self.tmp / f"f{i:02d}.txt"
            f.write_text("x\n")
            files += ["--attach", str(f)]
        before = self.ssh_log.read_text()
        p = self.rdd("send", "--to", "bob", *files, input="hi\n", ok=False)
        self.assertIn("at most 16", p.stderr)
        self.assertIn("one archive", p.stderr)
        self.assertEqual(self.ssh_log.read_text(), before)  # never connected
        (self.tmp / "a").mkdir()
        (self.tmp / "b").mkdir()
        (self.tmp / "a" / "log.txt").write_text("1\n")
        (self.tmp / "b" / "log.txt").write_text("2\n")
        odd = self.tmp / "report (1).pdf"
        odd.write_text("x\n")
        for files, said in (([self.tmp / "a" / "log.txt", self.tmp / "b" / "log.txt"], "two files named"), ([odd], "rename it")):
            p = self.rdd("send", "--to", "bob", *[w for f in files for w in ("--attach", str(f))], input="hi\n", ok=False)
            self.assertIn(said, p.stderr)
            self.assertEqual(self.ssh_log.read_text(), before)


class Mirror(RemoteBase):
    """--cached: the store's answers kept on the reader's machine, checked
    here on every read, refreshed by watch --mirror, dropped by a write."""

    def bob(self, **kw):
        return dict(DEADDROP_FROM="bob", **kw)

    def ids(self, *args, **env):
        return [m["id"] for m in json.loads(self.rdd("inbox", "--json", *args, env=self.bob(**env)).stdout)]

    def test_cached_reads_keep_the_answer_until_a_write_or_a_refresh(self):
        a = self.rdd("send", "--to", "bob", input="# A\n\nx\n").stdout.split()[1]
        self.assertEqual(self.ids("--cached"), [a])
        mirror = self.near / ".local" / "state" / "deaddrop" / "mirror"
        kept = [p for p in mirror.glob("*.json")]
        self.assertEqual(len(kept), 1)
        self.assertEqual(oct(kept[0].stat().st_mode & 0o777), "0o600")
        self.assertEqual(oct(mirror.stat().st_mode & 0o777), "0o700")
        # Something arrives that this machine didn't send: --cached still
        # answers from the mirror, with no ssh; without it, the store.
        b = self.dd("send", "--to", "bob", input="# B\n\nx\n", DEADDROP_DIR=str(self.far_store), DEADDROP_FROM="carol").stdout.split()[1]
        before = self.ssh_log.read_text()
        self.assertEqual(self.ids("--cached"), [a])
        self.assertEqual(self.ssh_log.read_text(), before)
        self.assertEqual(sorted(self.ids()), sorted([a, b]))
        # watch --mirror asks again for what's been read.
        c = self.dd("send", "--to", "bob", input="# C\n\nx\n", DEADDROP_DIR=str(self.far_store), DEADDROP_FROM="carol").stdout.split()[1]
        self.rdd("watch", "--mirror", "--no-hooks", env=self.bob())
        self.assertEqual(sorted(self.ids("--cached")), sorted([a, b, c]))
        # A write from here: the next cached read asks the store.
        self.rdd("mark", "read", a, env=self.bob())
        got = json.loads(self.rdd("inbox", "--json", "--all", "--cached", env=self.bob()).stdout)
        self.assertEqual({m["id"]: m["state"] for m in got}[a], "read")

    def test_a_write_during_the_first_cached_read_isnt_lost(self):
        # No mirror yet. While the first --cached read is with the store, this
        # machine marks the letter read: that answer is older than the write,
        # so the next cached read asks again.
        a = self.rdd("send", "--to", "bob", input="# A\n\nx\n").stdout.split()[1]
        once = self.tmp / "race-once"
        once.write_text("")
        near = {k: v for k, v in self.near_env(DEADDROP_FROM="bob").items() if v is not None}
        envs = " ".join(f"{k}='{v}'" for k, v in near.items())
        racer = script(self.tmp / "ssh-race", f'''
for last; do :; done
out=$("{self.ssh}" "$@")
case "$last" in *"'inbox'"*)
  if [ -e "{once}" ]; then
    rm "{once}"
    env {envs} DEADDROP_NOW="$DEADDROP_NOW" "{BIN}" mark read {a} >/dev/null
  fi;;
esac
printf '%s\n' "$out"
''')
        self.assertFalse((self.near / ".local" / "state" / "deaddrop" / "mirror").exists())
        args = ("inbox", "--json", "--all", "--cached")
        first = json.loads(self.rdd(*args, env=self.bob(DEADDROP_SSH=str(racer))).stdout)
        self.assertEqual({m["id"]: m["state"] for m in first}[a], "unread")  # as the store had it
        self.assertFalse(once.exists())
        got = json.loads(self.rdd(*args, env=self.bob()).stdout)
        self.assertEqual({m["id"]: m["state"] for m in got}[a], "read")

    def test_cached_reads_are_still_checked_here(self):
        signers(self.tmp / "signers", "alice")
        sk = dict(DEADDROP_SIGN_KEY=key("alice")[0], DEADDROP_SIGNERS=str(self.tmp / "signers"))
        id = self.rdd("send", "--to", "bob", input="# Signed\n\nx\n", env=sk).stdout.split()[1]
        self.assertEqual(json.loads(self.rdd("show", id, "--json", "--cached", env=self.bob(DEADDROP_SIGNERS=str(self.tmp / "signers"))).stdout)["signature"], "verified")
        # The same kept answer, read with a list that doesn't know alice.
        empty = self.tmp / "empty-signers"
        empty.write_text("")
        m = json.loads(self.rdd("show", id, "--json", "--cached", env=self.bob(DEADDROP_SIGNERS=str(empty))).stdout)
        self.assertNotEqual(m["signature"], "verified")

    def test_nothing_is_kept_without_asking(self):
        self.rdd("send", "--to", "bob", input="# A\n\nx\n")
        self.ids()
        self.rdd("watch", "--no-hooks", env=self.bob())
        self.assertFalse((self.near / ".local" / "state" / "deaddrop" / "mirror").exists())

    def test_a_planted_or_future_answer_is_never_used(self):
        self.rdd("send", "--to", "bob", input="# A\n\nx\n")
        self.ids("--cached")
        mirror = self.near / ".local" / "state" / "deaddrop" / "mirror"
        (entry,) = mirror.glob("*.json")
        e = json.loads(entry.read_text())
        # Dated ahead: not fresh, so the store is asked.
        e["fetched_at"] = "2099-01-01T00:00:00Z"
        entry.write_text(json.dumps(e))
        before = self.ssh_log.read_text()
        self.ids("--cached")
        self.assertNotEqual(self.ssh_log.read_text(), before)
        # Naming another store: a refresh never asks it, and lets it go.
        e = json.loads(entry.read_text())
        e["host"] = "elsewhere"
        entry.write_text(json.dumps(e))
        self.rdd("watch", "--mirror", "--no-hooks", env=self.bob())
        self.assertNotIn("elsewhere", self.ssh_log.read_text())
        self.assertFalse(entry.exists())


    def test_a_cached_read_is_heard_as_of_what_was_shown(self):
        from datetime import datetime
        epoch = lambda t: datetime.fromisoformat(t.replace("Z", "+00:00")).timestamp()
        self.rdd("send", "--to", "bob", input="# A\n\nx\n")
        heard = self.far_store / "bob" / ".heard"
        self.rdd("pull", "--json", "--cached", env=self.bob())  # fetched: heard
        heard.unlink()
        self.clock = at(1)
        self.rdd("watch", "--mirror", "--no-hooks", env=self.bob())  # a copy only, fetched at 1
        self.assertFalse(heard.exists())
        self.clock = at(2)
        self.dd("send", "--to", "bob", input="# B, after the copy\n\nx\n", DEADDROP_DIR=str(self.far_store), DEADDROP_FROM="carol")
        self.clock = at(3)
        self.rdd("pull", "--json", "--cached", env=self.bob())  # shown the copy from 1
        self.clock = at(4)
        self.rdd("watch", "--mirror", "--no-hooks", env=self.bob())  # tells the store: heard as of 1
        self.assertEqual(heard.stat().st_mtime, epoch(at(1)) - 1)  # a second back; B, at 2, is still unheard


class DefaultBox(RemoteBase):
    """A session with a box of its own ($DEADDROP_BOX, or the reply box it
    asks answers at) reads there by default, here and over ssh."""

    def test_reading_commands_default_to_the_sessions_box(self):
        a = self.rdd("send", "--to", "alice-helper", input="# For the helper\n\nx\n").stdout.split()[1]
        self.rdd("send", "--to", "alice", input="# For alice\n\nx\n", env=dict(DEADDROP_FROM="bob"))
        for var in ("DEADDROP_BOX", "DEADDROP_REPLY_BOX"):
            env = {var: "alice-helper"}
            self.assertEqual([m["id"] for m in json.loads(self.rdd("inbox", "--json", env=env).stdout)], [a])
            self.assertEqual([m["id"] for m in json.loads(self.rdd("pull", "--json", env=env).stdout)], [a])
            self.assertIn("For the helper", self.rdd("show", a, env=env).stdout)
        self.rdd("mark", "read", a, env=dict(DEADDROP_REPLY_BOX="alice-helper"))
        self.assertTrue((self.far_store / "alice-helper" / "read" / (a + ".md")).exists())
        # What you sent stays yours, and a named box still wins.
        self.assertIn("For the helper", self.rdd("sent", env=dict(DEADDROP_BOX="alice-helper")).stdout)
        self.assertIn("For alice", self.rdd("pull", "alice", env=dict(DEADDROP_BOX="alice-helper")).stdout)

    def test_a_letter_in_another_box_says_where(self):
        a = self.rdd("send", "--to", "alice-helper", input="# Q\n\nx\n").stdout.split()[1]
        self.rdd("send", "--to", "alice", input="# Mine\n\nx\n", env=dict(DEADDROP_FROM="bob"))
        p = self.rdd("mark", "read", a, ok=False)
        self.assertIn("--box alice-helper", p.stderr)



class Gate(RemoteBase):
    """A gated key: the store's login runs `deaddrop gate --as alice --read
    alice,alice-helper` for it, whatever the caller asks, and nothing else."""

    def setUp(self):
        super().setUp()
        # The stand-in for sshd with a forced command: the line the caller
        # sent goes to the gate as $SSH_ORIGINAL_COMMAND, never to a shell.
        self.ssh = __import__("_binary").script(self.tmp / "ssh-gated", f'''
printf '%s\\n' "$@" >> "{self.ssh_log}"
for last; do :; done
exec env -i PATH=/usr/bin:/bin HOME="{self.far}" DEADDROP_NOW="$DEADDROP_NOW" \\
  SSH_ORIGINAL_COMMAND="$last" DEADDROP_FROM=mallory "{self.far}/.local/bin/deaddrop" gate --as alice --read alice,alice-helper \\
  --dir "{self.far_store}" --config "{self.far}/.config/deaddrop/config.toml"
''')

    def far_send(self, box, text, **env):
        return self.dd("send", "--to", box, input=text, DEADDROP_DIR=str(self.far_store), DEADDROP_FROM=env.get("frm", "carol")).stdout.split()[1]

    def test_the_key_reads_its_own_boxes_and_sends_as_itself(self):
        a = self.far_send("alice", "# For alice\\n\\nx\\n")
        self.far_send("alice-helper", "# For the helper\\n\\nx\\n")
        self.assertEqual([m["id"] for m in json.loads(self.rdd("inbox", "--json").stdout)], [a])
        self.assertIn("For the helper", self.rdd("pull", "alice-helper").stdout)
        self.rdd("mark", "read", a)
        sent = self.rdd("send", "--to", "bob", input="# Hello\\n\\nx\\n").stdout
        self.assertRegex(sent, r"-alice-")  # as alice, though the far side was told mallory
        self.assertTrue((self.far_store / "alice" / "read" / (a + ".md")).exists())

    def test_the_key_reads_nothing_else(self):
        self.far_send("bob", "# Bob's\\n\\nx\\n")
        refused = [self.rdd("inbox", "bob", "--json", ok=False), self.rdd("pull", "--json", env=dict(DEADDROP_BOX="bob"), ok=False),
                   self.rdd("status", "--box", "bob", ok=False)]
        self.assertIn("reads only alice, alice-helper", refused[0].stderr)
        self.assertIn("names its boxes", self.rdd("status", ok=False).stderr)
        b = self.far_send("bob", "# Another\\n\\nx\\n")
        refused.append(self.rdd("mark", "read", b, "--box", "bob", ok=False))
        for p in refused:
            self.assertIn("reads only", p.stderr)
            self.assertNotIn("usage:", p.stderr)  # refused, not used wrong
            self.assertEqual(p.returncode, 5, p.stderr)

    def test_the_key_sends_only_as_itself_and_runs_only_the_store_commands(self):
        p = self.rdd("send", "--to", "bob", "--from", "automation", input="x\\n", ok=False)
        self.assertIn("sends only as alice", p.stderr)
        self.assertEqual(p.returncode, 5)
        for cmd in ("gc", "firetower", "follow-up"):
            p = self.rdd(cmd, ok=False)
            self.assertIn("can't run", p.stderr)
            self.assertEqual(p.returncode, 5)  # every gate refusal: one code

    def gate(self, line, argv0=None, input=None, extra_env=None, gate_args=()):
        """The gate run directly, as sshd would, with this line."""
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.far), "DEADDROP_NOW": self.clock}
        if line is not None:
            env["SSH_ORIGINAL_COMMAND"] = line
        env.update(extra_env or {})
        exe = argv0 or str(self.far / ".local" / "bin" / "deaddrop")
        # The line's one order: --signed, then --dir, then --config.
        extra = list(gate_args)
        signed = ["--signed"] if "--signed" in extra else []
        d = extra[extra.index("--dir") + 1] if "--dir" in extra else str(self.far_store)
        owner = (*signed, "--dir", d, "--config", str(self.far / ".config" / "deaddrop" / "config.toml"))
        return subprocess.run([exe, "gate", "--as", "alice", "--read", "alice,alice-helper", *owner], input=input,
                              capture_output=True, text=True, env=env, cwd=str(self.far / ".local" / "bin"))

    def test_file_behind_the_gate_hands_over_as_stored(self):
        # Like every other read behind the gate: the key holder's own
        # deaddrop checks the signature; the store neither judges nor keeps
        # a ledger on its behalf.
        att = self.tmp / "notes.txt"
        att.write_text("the attached bytes\n")
        id = self.dd("send", "--to", "alice", "--attach", str(att), input="# With a file\n\nx\n",
                     DEADDROP_DIR=str(self.far_store), DEADDROP_FROM="carol").stdout.split()[1]
        p = self.gate(f"deaddrop file {id} notes.txt")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(p.stdout, "the attached bytes\n")
        self.assertNotIn("isn't signed", p.stderr)  # the store didn't judge it
        # Still the file that was sent: a changed one isn't handed over.
        stored = next(self.far_store.rglob("notes.txt"))
        stored.write_text("changed\n")
        p = self.gate(f"deaddrop file {id} notes.txt")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("isn't the file that was sent", p.stderr)

    def test_a_gated_key_isnt_told_the_stores_details(self):
        # A missing attachment's path is the owner's, in gate.log; the key
        # gets a word.
        att = self.tmp / "notes.txt"
        att.write_text("x\n")
        id = self.dd("send", "--to", "alice", "--attach", str(att), input="# F\n\nx\n",
                     DEADDROP_DIR=str(self.far_store), DEADDROP_FROM="carol").stdout.split()[1]
        next(self.far_store.rglob("notes.txt")).unlink()
        p = self.gate(f"deaddrop file {id} notes.txt")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("isn't there to hand over", p.stderr)
        self.assertNotIn(str(self.far_store), p.stderr)
        self.assertIn(str(self.far_store), (self.far_store / "gate.log").read_text())
        # notify's output behind the gate: a Go test (ring_test.go), since the
        # gate runs the account's own crontinental, never a stand-in.

    def test_a_gated_error_never_names_the_stores_folder(self):
        # A letter the store can't read, asked for by id: the key is told
        # it's broken, never where the store keeps it.
        id = self.far_send("alice", "# Fine\n\nx\n")
        f = next(self.far_store.rglob(id + ".md"))
        f.write_text("not a message at all")
        p = self.gate(f"deaddrop show {id} --raw")
        self.assertNotEqual(p.returncode, 0)
        self.assertNotIn(str(self.far_store), p.stderr)
        self.assertNotIn(str(self.far), p.stderr)
        self.assertIn("<store>", p.stderr)
        self.assertIn(str(self.far_store), (self.far_store / "gate.log").read_text())

    def test_a_relative_store_in_the_config_is_still_hidden(self):
        # The gate takes the store by its full path, so errors spell it the
        # way it's hidden, whatever the config says.
        id = self.far_send("alice", "# Fine\n\nx\n")
        next(self.far_store.rglob(id + ".md")).write_text("not a message at all")
        cfg = self.tmp / "relative.toml"
        cfg.write_text('dir = "../share/deaddrop"\n')  # from the gate's folder: the same store
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.far), "DEADDROP_NOW": self.clock,
               "SSH_ORIGINAL_COMMAND": f"deaddrop show {id} --raw"}
        p = subprocess.run([str(self.far / ".local" / "bin" / "deaddrop"), "gate", "--as", "alice", "--read", "alice",
                            "--config", str(cfg)], capture_output=True, text=True, env=env, cwd=str(self.far / ".local" / "bin"))
        self.assertNotEqual(p.returncode, 0)
        self.assertNotIn("share/deaddrop", p.stderr)
        self.assertIn("<store>", p.stderr)

    def test_the_gate_line_is_taken_only_as_written_plainly(self):
        # What an installer reads in authorized_keys is exactly what the gate
        # does: nothing it would tidy is accepted.
        exe = str(self.far / ".local" / "bin" / "deaddrop")
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.far), "SSH_ORIGINAL_COMMAND": "deaddrop version"}
        d = str(self.far_store)
        for line, why in (
                (["--as", "Alice", "--read", "alice", "--dir", d], "a name in lowercase"),
                (["--as", "alice", "--read", "Alice", "--dir", d], "boxes in lowercase"),
                (["--as", "alice", "--read", "alice, bob", "--dir", d], "boxes in lowercase"),
                (["--as", "alice", "--read", "alice,alice", "--dir", d], "names alice twice"),
                (["--as", "alice", "--read", "alice,", "--dir", d], "boxes in lowercase"),
                (["--as", "alice", "--read", "alice", "--read", "tj", "--dir", d], "isn't part of the line"),
                (["--as=alice", "--read", "alice", "--dir", d], "in that order"),
                (["--as", "alice", "--read=alice", "--dir", d], "in that order"),
                (["--read", "alice", "--as", "alice", "--dir", d], "in that order"),
                (["--as", "alice", "--read", "alice", "--signed", "--signed", "--dir", d], "isn't part of the line"),
                (["--as", "alice", "--read", "alice", "--dir", d, "--signed"], "isn't part of the line"),
                (["--as", "alice", "--read", "alice", "--signed=1", "--dir", d], "isn't part of the line"),
                (["--as", "alice", "--read", "--signed"], "needs a value of its own")):
            p = subprocess.run([exe, "gate", *line], capture_output=True, text=True, env=env)
            self.assertEqual(p.returncode, 2, (line, p.stderr))
            self.assertIn(why, p.stderr, line)
        p = self.gate("deaddrop version")
        self.assertEqual(p.returncode, 0, p.stderr)  # the control: alice,alice-helper as written

    def signed_gate(self):
        """The gate with --signed on its line."""
        self.ssh = __import__("_binary").script(self.tmp / "ssh-gated-signed", f'''
for last; do :; done
exec env -i PATH=/usr/bin:/bin HOME="{self.far}" DEADDROP_NOW="$DEADDROP_NOW" \\
  SSH_ORIGINAL_COMMAND="$last" "{self.far}/.local/bin/deaddrop" gate --as alice --read alice,alice-helper --signed \\
  --dir "{self.far_store}" --config "{self.far}/.config/deaddrop/config.toml"
''')

    def test_signed_gate_refuses_an_unsigned_letter(self):
        self.signed_gate()
        cfg = self.far / ".config" / "deaddrop"
        cfg.mkdir(parents=True, exist_ok=True)
        __import__("_binary").signers(cfg / "allowed_signers", "alice", "bob")  # the store's list
        p = self.rdd("send", "--to", "bob", input="# Unsigned\n\nx\n", ok=False)
        self.assertEqual(p.returncode, 5, p.stderr)
        self.assertIn("this key's letters are signed", p.stderr)
        id = self.far_send("alice", "# To answer\n\nx\n")
        p = self.rdd("reply", id, input="an unsigned answer\n", ok=False)
        self.assertEqual(p.returncode, 5, p.stderr)
        # The control: signed by alice, both go.
        self.rdd("send", "--to", "bob", input="# Signed\n\nx\n", env=Remote.signed_env(self, "alice", DEADDROP_NOW=None))
        self.rdd("reply", id, input="a signed answer\n", env=Remote.signed_env(self, "alice", DEADDROP_NOW=None))
        ms = json.loads(self.dd("pull", "carol", "--json", DEADDROP_DIR=str(self.far_store)).stdout)
        self.assertEqual([m.get("signed_by") for m in ms], ["alice"])
        # Signed "alice" with bob's key: a good signature, the wrong key.
        forged = Remote.signed_env(self, "alice", DEADDROP_SIGN_KEY=__import__("_binary").key("bob")[0], DEADDROP_NOW=None)
        p = self.rdd("send", "--to", "carol", input="# Forged\n\nx\n", env=forged, ok=False)
        self.assertEqual(p.returncode, 5, p.stderr)
        self.assertIn("this one isn't accepted", p.stderr)
        # A letter signed long ago (the near clock is days behind): refused.
        p = self.rdd("send", "--to", "carol", input="# Old\n\nx\n", env=Remote.signed_env(self, "alice"), ok=False)
        self.assertEqual(p.returncode, 5, p.stderr)
        self.assertIn("sign it again", p.stderr)
        self.assertNotIn("Forged", self.dd("pull", "carol", DEADDROP_DIR=str(self.far_store)).stdout)

    def test_signed_gate_needs_the_stores_list(self):
        self.signed_gate()
        p = self.rdd("send", "--to", "bob", input="# Signed\n\nx\n", env=Remote.signed_env(self, "alice", DEADDROP_NOW=None), ok=False)
        self.assertEqual(p.returncode, 5, p.stderr)
        self.assertIn("has no key for alice", p.stderr)

    def test_follow_ups_a_key_books_are_capped_an_hour(self):
        to = [w for i in range(20) for w in ("--to", f"box{i}")]
        self.rdd("send", *to, "--follow-up", "1d", input="x\n")
        p = self.rdd("send", *to, "--follow-up", "1d", input="x\n", ok=False)
        self.assertEqual(p.returncode, 5, p.stderr)
        self.assertIn("20 follow-ups booked in the last hour", p.stderr)
        self.rdd("send", *to, input="x\n")  # without --follow-up: fine
        # The hour turning: a Go test (the gate keeps no DEADDROP_NOW).

    def test_the_gates_dir_is_written_plainly(self):
        # Errors spell paths cleaned; a --dir written otherwise wouldn't
        # match when the gate hides it.
        for d in (str(self.far_store) + "/", str(self.far_store).replace("/", "//", 1),
                  str(self.far_store.parent) + "/./" + self.far_store.name):
            p = self.gate("deaddrop status --box alice --count", gate_args=("--dir", d))
            self.assertEqual(p.returncode, 2, (d, p.stderr))
            self.assertIn("written plainly", p.stderr)

    def test_a_typed_line_may_have_extra_spaces(self):
        # A person at a shell types two spaces now and then; a shell reads
        # them as one, and so does the gate.
        self.far_send("alice", "# Hi\n\nx\n")
        for line in ("deaddrop  status --box alice --count", "deaddrop status  --box alice   --count  ",
                     "deaddrop status --box alice --count "):
            p = self.gate(line)
            self.assertEqual(p.returncode, 0, (line, p.stderr))
            self.assertEqual(p.stdout.split()[0], "1")
        p = self.gate("deaddrop  ")
        self.assertEqual(p.returncode, 5)
        self.assertIn("say which", p.stderr)

    def test_remote_modes_own_line_is_held_to_one_space(self):
        self.far_send("alice", "# Hi\n\nx\n")
        line = "d='deaddrop'; DEADDROP_HOPS=1 DEADDROP_FROM='alice' exec \"$d\" 'status'  '--box' 'alice' '--count'"
        p = self.gate(line)
        self.assertEqual(p.returncode, 5)
        self.assertIn("characters a shell would read", p.stderr)
        self.assertEqual(self.gate(line.replace("  ", " ")).returncode, 0)  # the control

    def test_an_empty_command_word_is_said(self):
        line = "d='deaddrop'; DEADDROP_HOPS=1 DEADDROP_FROM='alice' exec \"$d\" '' 'inbox'"
        p = self.gate(line)
        self.assertEqual(p.returncode, 5)
        self.assertIn("'' isn't one", p.stderr)
        self.assertNotIn("can't run :", p.stderr)

    def test_sent_shows_only_copies_in_readable_boxes(self):
        self.rdd("send", "--to", "alice-helper", "--to", "carol", input="# To two\\n\\nx\\n")
        rows = json.loads(self.rdd("sent", "--json").stdout)
        boxes = {c["box"] for r in rows for c in r.get("copies", [])}
        self.assertEqual(boxes, {"alice-helper"})  # the readable copy, and only it
        self.assertEqual([r.get("copies_partial") for r in rows], [True])  # said in the answer
        p = self.rdd("sent")
        self.assertIn("only letters with a copy in a box this key reads", p.stderr)  # said on the reader's machine
        p = self.gate("deaddrop sent --raw --json")
        self.assertIn("only letters with a copy in a box this key reads", p.stderr)

    def test_an_empty_sent_still_says_its_partial(self):
        # Everything alice sent went to boxes this key can't read: the list is
        # empty, and the holder is still told it's cut.
        self.rdd("send", "--to", "carol", input="# Only carol\n\nx\n")
        p = self.rdd("sent")
        self.assertIn("Nothing sent", p.stdout)
        self.assertIn("only letters with a copy in a box this key reads", p.stderr)
        self.assertEqual(json.loads(self.rdd("sent", "--json").stdout), [])  # the list's shape is unchanged

    def test_a_box_the_key_cant_read_ends_the_wait(self):
        self.far_send("carol", "# Carol's\n\nx\n")
        p = self.rdd("wait", "--box", "carol", ok=False, timeout=10)
        self.assertIn("this key reads only", p.stderr)
        self.assertEqual(p.returncode, 5)
        self.assertNotIn("still waiting", p.stderr)

    def test_a_bundle_over_the_caps_writes_nothing(self):
        # A key that builds its own stream: the store counts before writing.
        import io, tarfile
        self.far_send("alice", "# First\n\nx\n")
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as t:
            body = b"# Many\n\nx\n"
            ti = tarfile.TarInfo("body.md"); ti.size = len(body); t.addfile(ti, io.BytesIO(body))
            for i in range(17):
                ti = tarfile.TarInfo(f"files/f{i:02d}.txt"); ti.size = 2; t.addfile(ti, io.BytesIO(b"x\n"))
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.far), "DEADDROP_NOW": self.clock,
               "SSH_ORIGINAL_COMMAND": "deaddrop send --to alice --subject many --bundle"}
        p = subprocess.run([str(self.far / ".local" / "bin" / "deaddrop"), "gate", "--as", "alice", "--read", "alice",
                            "--dir", str(self.far_store), "--config", str(self.far / ".config" / "deaddrop" / "config.toml")],
                           input=buf.getvalue(), capture_output=True, env=env)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn(b"more than 16 attachments", p.stderr)
        self.assertEqual(list(self.far_store.glob(".incoming-*")), [])
        self.assertEqual(len(list((self.far_store / "alice" / "new").glob("*.md"))), 1)

    def test_attach_never_reads_the_stores_files(self):
        secret = self.far / "secret.txt"
        secret.write_text("the store's own file\\n")
        p = self.gate(f"deaddrop send --to alice --subject x --attach {secret}", input="x\\n")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("never --attach", p.stderr)

    def test_a_relaying_store_refuses_to_gate(self):
        cfg = self.far / ".config" / "deaddrop"
        cfg.mkdir(parents=True, exist_ok=True)
        (cfg / "config.toml").write_text('host = "elsewhere"\nrelay = true\n')
        p = self.gate("deaddrop inbox carol --raw --json")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("isn't the store", p.stderr)
        self.assertNotIn("elsewhere", p.stderr)  # which host is nobody's business

    def test_nothing_is_said_about_other_boxes(self):
        self.far_send("alice", "# Alice's own\\n\\nx\\n")
        c = self.far_send("carol", "# Carol's\\n\\nx\\n")
        # (The id names its sender; what mustn't show is where it is.)
        p = self.gate(f"deaddrop show {c} --fingerprint")
        self.assertNotEqual(p.returncode, 0)
        self.assertEqual(p.stdout.strip(), "")
        self.assertIn("anywhere in the store", p.stderr)
        self.assertNotIn("carol's box", p.stderr)
        p = self.gate(f"deaddrop mark read {c} --box alice")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("no message", p.stderr)
        self.assertNotIn("carol's box", p.stderr)
        p = self.gate("deaddrop send --to dave --reply-box zed --subject x", input="x\n")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertNotIn("zed", p.stderr)  # unsigned: answers go to the sender, and nothing is said of zed

    def test_a_flag_value_of_dashes_cant_hide_a_from(self):
        p = self.gate("deaddrop send --to bob --subject -- --from carol", input="x\n")
        self.assertNotEqual(p.returncode, 0)
        self.assertRegex(p.stderr, r"sends only as alice|send as yourself \(alice\)|got -- ")
        self.assertEqual(self.files("bob", "new"), [])
        p = self.gate("deaddrop send --to bob --subject=-- --from carol", input="x\n")
        self.assertNotEqual(p.returncode, 0)
        self.assertRegex(p.stderr, r"sends only as alice|send as yourself \(alice\)")

    def test_a_bare_dashes_and_from_as_a_filter_are_honest_use(self):
        self.far_send("alice", "# From carol\n\nx\n")
        p = self.gate("deaddrop pull --raw --json --from carol --")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(len(json.loads(p.stdout)), 1)

    def test_the_environment_says_nothing(self):
        elsewhere = self.tmp / "elsewhere-store"
        planted = self.tmp / "planted.toml"
        planted.write_text('host = "elsewhere"\nrelay = true\n')
        xdg = self.tmp / "xdg"
        (xdg / "deaddrop").mkdir(parents=True)
        (xdg / "deaddrop" / "config.toml").write_text('host = "elsewhere"\n')
        p = self.gate("deaddrop send --to alice --subject x", input="x\n",
                      extra_env={"DEADDROP_DIR": str(elsewhere), "DEADDROP_CONFIG": str(planted), "XDG_CONFIG_HOME": str(xdg),
                                 "XDG_DATA_HOME": str(elsewhere), "DEADDROP_FROM": "carol", "PATH": str(self.tmp)})
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertFalse(elsewhere.exists())
        self.assertRegex(p.stdout, r"-alice-")
        self.assertTrue(list((self.far_store / "alice" / "new").glob("*-alice-*.md")))

    def test_too_many_hops_is_refused(self):
        line = """d="$HOME/.local/bin/deaddrop"; [ -x "$d" ] || d=deaddrop; DEADDROP_HOPS=7 DEADDROP_FROM='x' exec "$d" 'inbox' '--raw' '--json'"""
        self.assertIn("is the limit", self.gate(line).stderr)

    def test_reads_are_raw_and_the_plain_form_works(self):
        self.far_send("alice", "# Hi\\n\\nx\\n")
        self.assertIn("--raw", self.gate("deaddrop inbox --json").stderr)
        p = self.gate("deaddrop inbox --raw --json")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(len(json.loads(p.stdout)), 1)

    def test_no_line_or_a_relative_path_is_refused(self):
        self.assertIn("remote mode only", self.gate(None).stderr)
        p = self.gate("deaddrop inbox --raw --json", argv0="./deaddrop")
        self.assertIn("full path", p.stderr)

    def test_the_owner_says_where_the_store_is(self):
        own = self.tmp / "srv-drop"
        p = self.gate("deaddrop send --to alice --subject x", input="x\n", gate_args=("--dir", str(own)))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertTrue(list((own / "alice" / "new").glob("*.md")))
        self.assertIn("full path", self.gate("deaddrop inbox --raw --json", gate_args=("--dir", "rel")).stderr)

    def test_a_broken_config_is_logged_for_the_owner(self):
        cfg = self.far / ".config" / "deaddrop"
        cfg.mkdir(parents=True, exist_ok=True)
        (cfg / "config.toml").write_text("not = [toml\n")
        p = self.gate("deaddrop inbox --raw --json")
        self.assertIn("can't be read", p.stderr)
        self.assertNotIn("toml", p.stderr)
        log = (self.far_store / "gate.log").read_text()
        self.assertIn("config can't be read", log)

    def test_every_gate_refusal_exits_5(self):
        self.far_send("alice", "# Hi\n\nx\n")
        self.far_send("carol", "# Carol's\n\nx\n")
        secret = self.far / "secret.txt"
        secret.write_text("x\n")
        hops = """d="$HOME/.local/bin/deaddrop"; [ -x "$d" ] || d=deaddrop; DEADDROP_HOPS=7 DEADDROP_FROM='x' exec "$d" 'inbox' '--raw' '--json'"""
        cases = {
            "attach": dict(line=f"deaddrop send --to alice --subject x --attach {secret}", input="x\n"),
            "a box": dict(line="deaddrop inbox carol --raw --json"),
            "not raw": dict(line="deaddrop inbox --json"),
            "no line": dict(line=None),
            "no command": dict(line="deaddrop"),
            "not a command": dict(line="deaddrop gc"),
            "hops": dict(line=hops),
            "relative path": dict(line="deaddrop inbox --raw --json", argv0="./deaddrop"),
            "from another": dict(line="deaddrop send --to bob --subject x --from carol", input="x\n"),
            "signed by another": dict(line="deaddrop send --to bob --subject x --signed-by carol --signed-at 2026-11-02T15:30:00Z --signature x", input="x\n"),
            "status without a box": dict(line="deaddrop status"),
        }
        for name, c in cases.items():
            line = c.pop("line")
            p = self.gate(line, **c)
            self.assertEqual(p.returncode, 5, (name, p.stderr))
            self.assertNotIn("usage:", p.stderr, name)
        # The config breaking is the owner's to fix: refused too.
        cfg = self.far / ".config" / "deaddrop"
        cfg.mkdir(parents=True, exist_ok=True)
        (cfg / "config.toml").write_text('host = "elsewhere"\n')
        p = self.gate("deaddrop inbox --raw --json")
        self.assertIn("isn't the store", p.stderr)
        self.assertEqual(p.returncode, 5)
        (cfg / "config.toml").write_text("not = [toml\n")
        self.assertEqual(self.gate("deaddrop inbox --raw --json").returncode, 5)

    def test_no_command_asks_for_one(self):
        self.assertIn("say which", self.gate("deaddrop").stderr)
