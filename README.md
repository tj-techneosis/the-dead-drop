# The Dead Drop

*Leave it at a known spot; someone collects it later.*

The Dead Drop is a shared, tagged inbox of Markdown documents, and a filing
system for each person's own automated reports:

- **People send each other documents.** A write-up, field notes from a test
  run, requests for the next version of a tool: dropped in another person's
  box with a few tags, instead of copied out of one session and pasted into
  another.
- **Each person files their own reports.** A job that runs after every run
  of a tool you're tuning, or every morning, ends in `deaddrop send` and
  files its report in your own box.

Later, a working session pulls in everything that matters for what it's
doing, uses it, and files it away.

```
$ deaddrop inbox
STATE   ID                                 URGENCY  FROM        SENT    TAGS                SUBJECT
unread  2026-11-02T091102-automation-c103  fyi      automation  6h ago  tool-y,report       tool-y run 41
unread  2026-11-02T153012-bob-3f9a         normal   bob         1m ago  tool-x,request      Requests for tool X v9
unread  2026-11-02T153344-bob-01ea         urgent   bob         just now tool-y,field-notes Weekend on the iOS apps
```

It only holds and hands out documents. It sends no email, pushes no
notifications and runs nothing on a timer by itself: a scheduler keeps its
status current, and a board ([the Firetower](https://github.com/tj-techneosis/the-firetower))
shows it.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/tj-techneosis/the-dead-drop/main/install.sh | bash
```

One binary, `~/.local/bin/deaddrop`, for macOS and Linux (arm64 and amd64).
From a checkout, `./install.sh` builds it (Go 1.26, or mise). Either way, if
Claude Code is set up, it links the `deaddrop` skill and offers a short
section for `CLAUDE.md`. `deaddrop upgrade` installs the latest release;
`./install.sh --uninstall` removes it and leaves the mail alone.

## Sending

The document is Markdown on stdin:

```sh
deaddrop send --to bob --tag tool-x --tag request < requests.md
deaddrop send --to alice --tag tool-y --tag field-notes --fyi < notes.md
deaddrop send --to me --from automation --tag tool-y --tag report --attach run.csv < run.md
```

| flag | |
|---|---|
| `--to WHO` | a box: any lowercase name, or `me`. Repeat it (or `--to a,b`) for copies to several |
| `--subject "…"` | without it, the document's first heading (or its first line) |
| `--tag T` | repeatable; lowercase. The tool or project first, then the kind of document |
| `--urgent`, `--fyi` | the urgency; normal without either |
| `--attach FILE` | repeatable; fetched later with `deaddrop file ID NAME` |
| `--reply-to ID` | what it answers (`deaddrop reply ID` does this for you, back to the sender) |
| `--follow-up 2d` | raise it to urgent if it's still unread then (a span or a time) |
| `--from automation` | a job's reports. You send as yourself or as `automation`, never as someone else |

**Never put a secret in a message.** Everyone who can read the store can
read every box. Name where the secret lives instead. `send` refuses the
shapes that are never anything but a secret (private keys, cloud and API
tokens), but it can't catch everything.

## Collecting

```sh
deaddrop inbox                         # your unread mail (--all: read and filed too)
deaddrop pull --tag tool-y --from automation --from bob
deaddrop mark archive ID ID ...        # file what you used
```

`pull` prints every matching message in full, unread first and oldest first
within that, each between `===== deaddrop xxxx: … =====` lines (a random
mark per pull, so a document can't fake one), and ends with the ids and the
`mark` command to file them. `--json` gives the same to a program.
`--unread` leaves out mail already read; `--since 7d` or `--since
2026-11-01` leaves out older mail.

**`pull` marks nothing.** A session that crashes halfway loses no mail.
When the work is done, `mark` files it: `read` (dealt with; still pulled),
`archive` (filed; out of the way, gone after 30 days) or `unread` (put
back). `mark` takes ids, or `--tag` and `--from` to pick by those.

Several `--tag` all have to match; several `--from` match any of them. So
"tool Y's automated reports and anything from Bob about tool Y" is
`--tag tool-y --from automation --from bob`.

## Who you are

You are `$DEADDROP_FROM`, else `me` in `~/.config/deaddrop/config.toml`,
else your login name. That's the default sender, the box `inbox`, `pull`
and `mark` read, and what `--to me` means. `automation` is a sender, never
a box: a job sends `--from automation --to` its owner.

On a machine where everyone logs in as the same user, the login name says
nothing. Put `shared_login = true` in that machine's config and the
login name is never used: without `DEADDROP_FROM`, anything that needs to
know who you are refuses to run instead of filing mail under the shared
login. (A job that sends `--from automation --to <someone>` needs no one.)

## One store, many machines

The store lives on one machine everyone can reach:

```
~/.local/share/deaddrop/          ($DEADDROP_DIR)
  README.md                       what this folder is, and: no secrets
  <who>/new/<id>.md               unread
  <who>/read/<id>.md              read
  <who>/archive/<id>.md           filed
  <who>/files/<id>/…              attachments that came with a message
```

Every message is one file: a small header, then the document.

```markdown
---
id: 2026-11-02T153012-alice-3f9a
from: alice
to: bob
subject: "Requests for tool X v9"
tags: [tool-x, request]
urgency: normal
thread: 2026-10-30T091102-bob-77c2
follow_up: 2026-11-04T09:00:00Z
sent: 2026-11-02T15:30:12Z
---
The document itself.
```

Ids sort by time and say who sent them. Moving a file between `new`,
`read` and `archive` is the only change of state. Writes are atomic (a
temp file, then a rename), so nobody sees half a message.

On any other machine, set the store's host, and every command runs there
over ssh:

```toml
# ~/.config/deaddrop/config.toml
me = "alice"
host = "store-box"
```

Bodies and attachments go over ssh's stdin; nothing is written on the
calling machine except what `pull` prints. Everyone may log in to the
store's machine as the same user, so the login name there says nothing:
the caller's own name always goes across, and remote mode refuses to run
without one. `examples/config.toml` has every setting.

## On the board

```sh
deaddrop firetower
```

reports one lookout per box to the Firetower on the store's machine,
`mail/<who>/inbox`:

| box | lookout |
|---|---|
| empty | clear, "No mail." |
| unread | smoke, "3 unread" |
| urgent unread | fire, "1 urgent, 3 unread" |

The board says "moved" when new mail comes in. Only counts go to the board,
never subjects or senders, because boards can be on screens anyone sees. A
scheduler runs it (see `examples/store-jobs.sh`): under Crontinental it
takes the next due time from `$CRONTINENTAL_NEXT`, so the board shows "no
word" if the job stops.

`deaddrop status` shows the same per box in the terminal, with the newest
unread subject.

## Follow-ups and rings

`send --follow-up 2d` books a one-off [Crontinental](https://github.com/Techneosis/the-crontinental)
job on the store's machine. When it comes due, it raises the message to
urgent if it's still unread, and does nothing if it was read. Marking the
message read or archived cancels the job. `ring_urgent = true` in the
store machine's config also runs `crontinental notify --urgent` for urgent
mail, which rings whatever notify hooks are set up there. It's off by
default, and like the board it never says the subject or the sender.

## Reports of your own

The Dead Drop has no report feature. A report is any job that ends in
`deaddrop send`:

```sh
crontinental every 08:00 --name tower-report --forever --notify never -- \
  'firetower ls --tower T | deaddrop send --to alice --from automation --tag T --tag report --fyi --subject "T daily"'
```

What gets reported, and when, belongs to whoever books it.

## Everything

```
deaddrop send --to WHO [--subject S] [--tag T]... [--urgent|--fyi] [--reply-to ID]
              [--attach FILE]... [--follow-up 2d|TIME] [--from automation] < body.md
deaddrop reply ID [--urgent|--fyi] [--tag T]... [--follow-up 2d|TIME] < body.md
deaddrop inbox [WHO] [--tag T]... [--from WHO]... [--since …] [--all] [--json]
deaddrop pull  [WHO] [--tag T]... [--from WHO]... [--since …] [--unread] [--json]
deaddrop file ID [NAME] [--box WHO]
deaddrop mark read|unread|archive ID... | --tag T... [--from WHO]... [--box WHO]
deaddrop status [--json]
deaddrop firetower [--next 6m|TIME|none] [--box WHO]...
deaddrop gc [--days 30]
deaddrop upgrade [--check] | version | help
```

## Not here (yet)

Real email, a web inbox, boxes for people outside the team, permissions per
box (every box is readable by anyone who can read the store), search beyond
tags, sender and date, encryption at rest.

## License

MIT
