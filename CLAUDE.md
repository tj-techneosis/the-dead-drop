# Working on the Dead Drop

This file is for working **on** the Dead Drop. The instructions for **using**
it (what an agent does when asked to send or pull mail) are the skill in
`agents/skills/deaddrop/SKILL.md`, which `install.sh` links into a user's
Claude Code setup.

## Layout

| path | what |
|---|---|
| `cmd/deaddrop/` | the whole tool, in Go: one static binary (`CGO_ENABLED=0`) |
| `cmd/deaddrop/message.go` | the file format, ids, names, tags, subjects, the secret shapes, spans |
| `cmd/deaddrop/store.go` | the files: boxes, states, the box lock, atomic writes, moves |
| `cmd/deaddrop/commands.go` | send, reply, inbox, pull, file, mark, status, gc |
| `cmd/deaddrop/remote.go` | remote mode: every command over ssh, the caller's name across |
| `cmd/deaddrop/followup.go` | follow-ups (Crontinental jobs), the ring |
| `cmd/deaddrop/firetower.go` | `deaddrop firetower`: one lookout per box |
| `agents/` | what ships to users' Claude Code: the skill and the CLAUDE.md snippet |
| `examples/` | a config, the store machine's jobs |
| `install.sh` | three modes: piped fetches a release, a checkout builds, an unpacked release installs its binary |
| `scripts/package.sh` | builds and packages every release target, plus `SHA256SUMS` and `VERSION` |
| `tests/` | `python3 -m unittest discover -s tests`: run before every commit (it builds the binary) |
| `docs/design.md` | the design: why each piece is the way it is |

## Rules

- **Run the tests.** They are black-box and drive the real CLI against a
  throwaway store and config (`DEADDROP_DIR`, `DEADDROP_CONFIG`) with a
  pinned clock (`DEADDROP_NOW`), and stand-ins for ssh, Crontinental and the
  Firetower (`DEADDROP_SSH`, `DEADDROP_CRONTINENTAL`, `DEADDROP_FIRETOWER`),
  so they never touch the machine's own mail, jobs or board.
  `DEADDROP_BIN` points them at any build. A behaviour change comes with a
  test that fails without it.
- **One static binary.** The standard library and BurntSushi/toml.
- **The message file is a contract.** People and other tools read it.
  Header fields are added, never renamed or repurposed. The CLI is the only
  writer.
- **State is where the file is.** new, read, archive; a rename is the only
  change of state. Every change of state holds the box's lock. Nothing runs
  in the background.
- **No clock.** The Dead Drop schedules nothing: follow-ups are Crontinental
  jobs, the board is kept current by a scheduled `deaddrop firetower`.
- **`pull` never marks.** A crashed session must not lose mail.
- **Who sends.** `--from` is yourself or `automation`; with `shared_login`
  the login name is never a fallback. Jobs the Dead Drop books run the
  installed binary (`~/.local/bin`), never the one that booked them.
- **Nothing private on a board or a ring.** Counts and a hash, never a
  subject, a sender or an id (ids name their sender).
- **Never a secret in a message**, and the store's README and the skill say
  so. The secret shapes in `message.go` are a backstop, not a scanner: only
  shapes that are never anything else, so a document *about* secrets goes
  through.
- **The installer never touches the store.** Its files live in
  `~/.local/share/deaddrop-install`; the store's default is
  `~/.local/share/deaddrop`, and `--uninstall` leaves it.
- **This repo is public.** Nothing about any one person, client, machine
  or project in any file, commit message or PR: examples use `alice`, `bob`,
  `tool-x`, `store-box`. The tool hires no automations: what sends mail
  lives with whatever sends it. Mail is runtime data, never in git.
- **Commit messages carry no AI attribution.**

## Cutting a release

1. `git tag vX.Y.Z && git push origin vX.Y.Z`. A tag with a `-` publishes as
   a prerelease, which the installer's "latest" skips.
2. `.github/workflows/release.yml` packages all four targets, verifies them on
   Linux amd64, Linux arm64 and macOS (checksums, the version, the full suite
   against the packaged binary, the one-liner end to end), and only then
   publishes.

To rehearse locally: `scripts/package.sh v0.0.0-test /tmp/rel/latest/download`
and `DEADDROP_RELEASES=file:///tmp/rel bash -s -- --no-claude < install.sh`
(with a throwaway `HOME`).
