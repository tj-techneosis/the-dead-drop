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
        p = self.rdd("mark", "read", "2026-11-02T153000-bob-0000", ok=False)
        self.assertIn("no message", p.stderr)
        self.assertEqual(p.returncode, 1)
        unreachable = script(self.tmp / "ssh-down", "echo 'ssh: connect to host store-box: Connection refused' >&2; exit 255\n")
        p = self.rdd("status", env=dict(DEADDROP_SSH=str(unreachable)), ok=False)
        self.assertIn("could not reach the store on store-box", p.stderr)

    def test_ssh_is_batch_and_no_stdin_unless_needed(self):
        self.rdd("status")
        args = self.ssh_log.read_text().splitlines()
        self.assertIn("BatchMode=yes", args)
        self.assertIn("-n", args)

    def test_remote_bin(self):
        (self.near / "config.toml").write_text(f'host = "store-box"\nremote_bin = "{BIN}"\n')
        os.remove(self.far / ".local" / "bin" / "deaddrop")
        self.rdd("send", "--to", "bob", input="x\n", env=dict(DEADDROP_HOST=None))
        self.assertEqual(len(list((self.far_store / "bob" / "new").glob("*.md"))), 1)

    def test_local_wins_on_the_store_machine(self):
        # The far side is told DEADDROP_LOCAL=1, so its own host setting
        # can't send it round again.
        (self.far / ".config" / "deaddrop").mkdir(parents=True)
        (self.far / ".config" / "deaddrop" / "config.toml").write_text('host = "elsewhere"\n')
        self.rdd("send", "--to", "bob", input="x\n")
        self.assertEqual(len(list((self.far_store / "bob" / "new").glob("*.md"))), 1)
