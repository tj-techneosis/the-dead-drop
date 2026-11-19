"""Signatures are checked in-process, without ssh-keygen, whenever the answer
is sure, and give exactly the answer ssh-keygen gives. Whatever the fast path
doesn't read in full (a certificate authority, a key near its valid time, a
signature that doesn't hold) goes to ssh-keygen, as before."""

import atexit
import json
import os
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

from _binary import DeadDrop, key, script

_TYPED = {}


def typed_key(name: str, typ: str, *args) -> tuple:
    """A key of the given type, and its public line (type and base64)."""
    k = (name, typ)
    if k not in _TYPED:
        d = tempfile.mkdtemp(prefix="deaddrop-key-")
        atexit.register(shutil.rmtree, d, ignore_errors=True)
        path = os.path.join(d, f"id_{name}_{typ}")
        subprocess.run(["ssh-keygen", "-q", "-t", typ, *args, "-N", "", "-C", name, "-f", path], check=True)
        _TYPED[k] = (path, " ".join(Path(path + ".pub").read_text().split()[:2]))
    return _TYPED[k]


def ecdsa_available() -> bool:
    try:
        typed_key("probe", "ecdsa")
        return True
    except subprocess.CalledProcessError:
        return False


class InProcess(DeadDrop):
    def setUp(self):
        super().setUp()
        self.list = self.tmp / "allowed_signers"
        self.list.write_text("")
        self.log = self.tmp / "keygen.log"
        # Counts every ssh-keygen a reader runs, by what it was asked.
        self.shim = script(self.tmp / "ssh-keygen", f'echo "$2" >> "{self.log}"\nexec ssh-keygen "$@"\n')

    def keys(self):
        ks = {"ed25519": typed_key("alice", "ed25519"), "rsa": typed_key("alice", "rsa", "-b", "3072")}
        if ecdsa_available():
            ks["ecdsa"] = typed_key("alice", "ecdsa")
        return ks

    def read(self, inproc=True, **env):
        """bob's mail as (id, state, note), and the ssh-keygens that ran."""
        if self.log.exists():
            self.log.unlink()
        extra = dict(DEADDROP_SIGNERS=str(self.list), DEADDROP_NO_VCACHE="1", DEADDROP_SSH_KEYGEN=str(self.shim))
        if not inproc:
            extra["DEADDROP_NO_INPROC"] = "1"
        extra.update(env)
        ms = self.inbox("bob", DEADDROP_FROM="bob", **extra)
        runs = self.log.read_text().split() if self.log.exists() else []
        return {m["subject"]: (m["signature"], m["signature_note"]) for m in ms}, runs

    def same(self, *lines):
        """With the list as given: the fast path's answers are ssh-keygen's,
        letter by letter. Returns them, and what the fast path ran."""
        self.list.write_text("".join(l + "\n" for l in lines))
        fast, runs = self.read()
        slow, slow_runs = self.read(inproc=False)
        self.assertEqual(fast, slow, "\n".join(lines))
        self.assertTrue(slow_runs)  # the slow path really is ssh-keygen
        return fast, runs

    def test_the_fast_path_answers_as_ssh_keygen_does(self):
        ks = self.keys()
        bob_key = typed_key("bob", "ed25519")
        mallory = typed_key("mallory", "ed25519")
        for typ, (path, _) in ks.items():
            self.send(f"# {typ}\n\nx\n", "--to", "bob", "--subject", typ, DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=path)
        self.send("# tampered\n\nx\n", "--to", "bob", "--subject", "tampered", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=ks["ed25519"][0])
        f = next(p for p in (self.dir / "bob" / "new").glob("*.md") if "tampered" in p.read_text())
        f.write_text(f.read_text().replace("x\n", "y\n"))
        self.send("# bob's key\n\nx\n", "--to", "bob", "--subject", "wrong-signer", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=bob_key[0])
        self.send("# nobody's\n\nx\n", "--to", "bob", "--subject", "unlisted", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=mallory[0])
        plain = [f"alice {pub}" for _, pub in ks.values()] + [f"bob {bob_key[1]}"]

        # Plain lines: every good letter verified, here, with no ssh-keygen.
        got, runs = self.same(*plain)
        for typ in ks:
            self.assertEqual(got[typ][0], "verified", typ)
        self.assertEqual(got["tampered"][0], "bad")
        self.assertIn("changed after it was signed", got["tampered"][1])
        self.assertEqual(got["wrong-signer"][0], "bad")
        self.assertIn("the key is bob's", got["wrong-signer"][1])
        self.assertEqual(got["unlisted"][0], "bad")  # alice is listed, the key isn't hers
        self.assertIn("isn't alice's key", got["unlisted"][1])
        # ssh-keygen ran only for the two that don't hold by their listed
        # key (a verify, then whose key it is); never for a good one, nor for
        # a key nobody's list has.
        self.assertEqual(sorted(runs), sorted(["verify", "find-principals"] * 2), runs)

        # The same keys under the namespace, a glob, a comma list, and times
        # well inside their window: all still read here.
        got, runs = self.same(*[f'al*,carol namespaces="deaddrop",valid-after="20200101",valid-before="20990101Z" {pub}' for _, pub in ks.values()],
                              f"bob {bob_key[1]}")
        self.assertEqual([got[t][0] for t in ks], ["verified"] * len(ks))
        self.assertEqual(runs.count("verify"), 2, runs)

        # The principal isn't alice's: nobody vouches; who the key is.
        got, _ = self.same(*[f"carol {pub}" for _, pub in ks.values()])
        self.assertEqual(got["ed25519"][0], "bad")
        self.assertIn("the key is carol's", got["ed25519"][1])
        # Negated.
        got, _ = self.same(*[f"a*,!alice {pub}" for _, pub in ks.values()])
        self.assertNotEqual(got["ed25519"][0], "verified")
        # Another namespace.
        got, _ = self.same(*[f'alice namespaces="other" {pub}' for _, pub in ks.values()])
        self.assertEqual(got["ed25519"][0], "bad")
        # Expired, and not yet valid.
        for opt in ('valid-before="20200101"', 'valid-after="20990101"'):
            got, _ = self.same(*[f"alice {opt} {pub}" for _, pub in ks.values()])
            self.assertNotEqual(got["ed25519"][0], "verified", opt)
        # A key within a day of its time is ssh-keygen's to call.
        soon = time.strftime("%Y%m%d%H%M%SZ", time.gmtime(time.time() + 3600))
        got, runs = self.same(*[f'alice valid-before="{soon}" {pub}' for _, pub in ks.values()])
        self.assertEqual(got["ed25519"][0], "verified")
        self.assertIn("verify", runs)
        # A list with a certificate authority on it goes to ssh-keygen whole.
        ca = typed_key("ca", "ed25519")
        got, runs = self.same(f"*@example.org cert-authority {ca[1]}", *plain)
        self.assertEqual([got[t][0] for t in ks], ["verified"] * len(ks))
        self.assertGreaterEqual(runs.count("verify"), len(ks))
        # As does a line it can't read, or an option it doesn't know.
        got, runs = self.same("carol garbage", *plain)
        self.assertGreaterEqual(runs.count("verify"), len(ks))
        got, runs = self.same(*[f'alice no-touch-required {pub}' for _, pub in ks.values()])
        self.assertGreaterEqual(runs.count("verify"), len(ks))

    def test_a_key_nobody_lists_is_unknown_without_ssh_keygen(self):
        mallory = typed_key("mallory", "ed25519")
        self.send("# Hi\n\nx\n", "--to", "bob", "--subject", "m", DEADDROP_FROM="mallory", DEADDROP_SIGN_KEY=mallory[0])
        got, runs = self.same(f"alice {typed_key('alice', 'ed25519')[1]}")
        self.assertEqual(got["m"][0], "unknown")
        self.assertEqual(runs, [])

    def test_five_hundred_letters_need_no_ssh_keygen(self):
        k = typed_key("alice", "ed25519")
        self.list.write_text(f'alice namespaces="deaddrop" {k[1]}\n')
        for i in range(500):
            self.send(f"# {i}\n\nx\n", "--to", "bob", "--subject", f"n{i}", DEADDROP_FROM="alice", DEADDROP_SIGN_KEY=k[0])
        t = time.monotonic()
        got, runs = self.read()
        fast = time.monotonic() - t
        self.assertEqual(len(got), 500)
        self.assertEqual({s for s, _ in got.values()}, {"verified"})
        self.assertEqual(runs, [])
        self.assertLess(fast, 5.0)
