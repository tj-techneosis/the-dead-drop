"""Remote mode: every command runs on the store's machine over ssh, as the
caller, and nothing is written on the caller's side.

The far machine is a second HOME on this one, reached through a stand-in for
ssh that runs the command line it is given there, with a clean environment,
the way sshd would."""

import json
import os
from pathlib import Path

from _binary import BIN, DeadDrop, script


class Remote(DeadDrop):
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
        return sorted(str(p.relative_to(self.near)) for p in self.near.rglob("*"))

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
        p = self.rdd("send", "--to", "bob", input="x\n", env=dict(DEADDROP_FROM=None, USER="tec"), ok=False)
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
        self.assertIn("no message", p.stderr)
        self.assertEqual(p.returncode, 1)
        unreachable = script(self.tmp / "ssh-down", "echo 'ssh: connect to host store-box: Connection refused' >&2; exit 255\n")
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
