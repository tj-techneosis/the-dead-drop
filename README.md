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

It holds and hands out documents, and gives you what you need to see your
mail without a terminal: an inbox file on your own machine, hooks that run
when mail arrives, and menu-bar inboxes for macOS and Omarchy that read it.
It sends no email. A scheduler keeps it current (or `deaddrop watch --every`
does, if you have none), and a board ([the Firetower](https://github.com/tj-techneosis/the-firetower))
can show it.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/tj-techneosis/the-dead-drop/main/install.sh | bash
```

One binary, `~/.local/bin/deaddrop`, for macOS and Linux (arm64 and amd64).
From a checkout, `./install.sh` builds it (Go 1.26, or mise). Either way, if
Claude Code is set up, it links the `deaddrop` skill and offers a short
section for `CLAUDE.md`. `deaddrop upgrade` installs the latest release;
`./install.sh --uninstall` removes it and leaves the mail alone.

Upgrade the store's machine first, then the machines that use it, and add a
new setting to a config only once that machine's `deaddrop` knows it: a
config with a setting it doesn't know stops every command there. (From 0.3,
a machine reaching the store also needs the store on 0.3 or later, to hand
out signed messages. From 0.4: a reader older than 0.4 doesn't know
personas, and reads a signed message with one as bad; upgrade every machine
that reads mail before anyone sends `--as`. Mail `--for` a helper (0.6) is
filed where older versions never look, so they can't misread it.)

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

`deaddrop reply ID` sends back to the sender with "Re: " and the original's
tags, the ones about what it's about: tags starting `persona-` name who was
speaking, so they stay behind, and `--untag T` leaves any other (a proposal
answered with a status: `--untag proposal --tag status`).
| `--follow-up 2d` | raise it to urgent if it's still unread then (a span or a time) |
| `--from automation` | a job's reports. You send as yourself or as `automation`, never as someone else |
| `--as NAME` | one of your assistants is writing for you: a label under your name, like `--as "Release manager"`. `$DEADDROP_AS` sets it for a whole session |
| `--for HELPER` | for one of the recipient's helpers, into its own pigeonhole in their box (repeatable). See below |
| `--reply-box BOX` | answers go to that box, not yours: an assistant with its own box (`alice-helper`). Sealed; `reply` honours it only when the letter verifies. `$DEADDROP_REPLY_BOX` sets it for a session |

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
The JSON from `pull`, `inbox` and `show` is a contract other people's
panels read: fields are added, never renamed or repurposed.
`--unread` leaves out mail already read; `--since 7d` or `--since
2026-11-01` leaves out older mail.

**`pull` marks nothing.** A session that crashes halfway loses no mail.
When the work is done, `mark` files it: `read` (dealt with; still pulled),
`archive` (filed; out of the way, gone after 30 days) or `unread` (put
back). `mark` takes ids, or `--tag` and `--from` to pick by those.

Several `--tag` all have to match; several `--from` match any of them. So
"tool Y's automated reports and anything from Bob about tool Y" is
`--tag tool-y --from automation --from bob`.

**What you sent.** Each message is stored in its recipients' boxes, so
`deaddrop sent` puts it back together for you: everything you sent, oldest
first, one entry per message, with where each copy is and whether it's been
read.

```sh
deaddrop sent --to bob --tag tool-x
deaddrop sent --json            # the fields of pull --json, state "sent", plus:
                                # "copies": [{"box": "bob", "id": "…", "state": "read"}]
```

`--to` matches any recipient, `--tag` and `--since` work as in `pull`, and
`--box WHO` is for a front end acting for an identified person (like
`mark --box`). It never lists mail you received. Archived copies go after 30
days, and leave `copies` when they do. Together with `pull`, it's both
sides of a conversation.

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

## Who sent it: signed messages

A name is only a claim. Everyone may reach the store as the same login, and
anyone on it can set `DEADDROP_FROM`, or write a file straight into a box.
So messages are **signed with the sender's own ssh key, on the sender's own
machine**, and **checked on the reader's own machine**, against the reader's
own list of who holds which key. A key never goes near the store, so
nothing there can sign as you, and the store's own list never counts.

Set it up once per machine:

```toml
# ~/.config/deaddrop/config.toml
sign_key = "~/.ssh/id_ed25519"      # or its .pub, to sign through the ssh agent
```

```sh
deaddrop key      # this machine's line for everyone's list:
                  # alice namespaces="deaddrop" ssh-ed25519 AAAA…
```

Everyone keeps the same list, `~/.config/deaddrop/allowed_signers` (or
`signers = "…"` in the config): one line per person, from `deaddrop key`.
Keep it somewhere the store's login can't change, such as your own dotfiles.

Every message you read then says who sent it, as far as your machine can
tell:

| | `inbox` | `pull`, `show` |
|---|---|---|
| verified | ✓ | `[signature: verified, signed by alice]` |
| unsigned | - | the sender is only a claim |
| can't check | ? | signed, but your list has no key for them |
| **bad** | ✗ | changed since it was signed, delivered somewhere it wasn't sent, or signed with a key that isn't theirs |

What's signed: the sender (and persona), the recipients, subject, tags, urgency, thread,
each attachment's sha256, when it was signed, and the text. `file` refuses
an attachment that isn't the one sent, and any name that isn't a plain file
name. A verified letter says when it was signed, so one delivered again
later shows its age; the store's own delivery time is checked against it
too, but someone who can edit the store's files can change that, so the
signing time is the one to trust. A follow-up that raised a letter to
urgent says so: the sender's own urgency is what was signed. A job's `--from automation` mail is
signed by whose machine it runs on (`sign_as` to name someone else). On a
machine with `shared_login = true` the key belongs to the machine, not a
person: it never signs as anyone, so a front end there sending for someone
sends unsigned, and it signs automation mail only as its `sign_as`. The
JSON of `inbox`, `pull` and `show` carries `signature` (`verified`,
`unsigned`, `unknown` or `bad`), `signature_note` and `signed_by`, and so do
the inbox file, the hooks (`DEADDROP_MSG_SIGNATURE`) and both menu bars.

**Treat anything not verified as a claim.** Never act on a request because
of who an unverified message says it's from.

### Personas: assistants speaking for you

An assistant or a robot that writes for you sends as you, with a label:
`--as "Release manager"`, or `DEADDROP_AS` set once for its session. It's
your box and your key; the persona says which of your helpers is talking,
everywhere the sender shows (`alice, as Release manager`). It's signed with
the message, so it can't be added, changed or taken off without the
signature failing. It's never a sender of its own: replies go to you.

### Helpers: a pigeonhole each

People who hand work to several assistants want each one to see only its
own mail. So a box has a pigeonhole per helper, besides the person's own:

```sh
deaddrop send --to bob --for planner --for reviewer < plan.md   # one letter, two helpers
deaddrop inbox --for planner          # the planner's queue (DEADDROP_FOR=planner, once)
deaddrop inbox --for '*'              # every pigeonhole in the box, for whoever's minding it
deaddrop mark read ID                 # by id, wherever it is; by --tag, in the --for pigeonhole
deaddrop watch --for planner          # its own inbox file, its own hooks (DEADDROP_MSG_FOR)
```

- **A helper exists once something's addressed to it**, like a box: no
  list, no setup. Mail with no `--for` is the person's own, and their
  `inbox` stays theirs.
- **Each helper has its own copy, with its own state.** When one files a
  letter, it's still unread for the others. It's one letter: `sent`,
  `thread` and `threads` put the copies together.
- **The address is sealed into the signature.** A copy moved to another
  pigeonhole, or with a `for` added or taken off, reads as bad.
- **Your own inbox and `status` say when a helper has unread mail,** and
  sending to a pigeonhole nobody has used yet says so (a typo?).
- **Replies find their way back.** Answering "bob, as Release manager"
  (when that persona is signed and verified: anyone can claim one on an
  unsigned letter) goes to bob's `release-manager` pigeonhole (a persona's pigeonhole is its
  name, lowercase, with dashes), unless you say `--for`. What a letter was
  for is never copied back.
- `inbox`, `pull`, `show`, `file`, `mark`, `watch`, `bar`, `thread`,
  `with` and `threads` take `--for`, or `$DEADDROP_FOR` for a helper's
  whole session.

**Every thread at a glance.** `deaddrop threads` lists them, newest activity
first: subject, when it started and last moved, how many letters, how many
unread, every tag, and who's in it (box, persona, helper, and the least
trusted of their signatures). A reply names the copy its sender read, whose
id differs in every box, so the store also hands out each letter's
fingerprint (`letter`, and `thread_letter` for what it answers), and
threads join on those: every recipient sees the same thread. `--letters`
(and `--bodies`) adds the letters,
`--since` and `--limit` page it, `--json` for a front end.

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
recipients: [bob]
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
calling machine except what `pull` prints, and, if you run `deaddrop watch`,
the inbox file (no message text). Everyone may log in to the
store's machine as the same user, so the login name there says nothing:
the caller's own name always goes across, and remote mode refuses to run
without one. `examples/config.toml` has every setting.

**Through another machine.** If you can only reach the store's machine
through a second one, point your `host` at the second one, and put
`relay = true` in *its* config (beside its own `host`). Commands that reach
it over ssh then go on to the store, still as you, attachments and all. A
command passes through at most three machines, so a relay that points back
at itself stops instead of looping. Without `relay`, a machine with a `host`
of its own refuses a command that arrives over ssh, and says to set `relay`,
rather than answering from an empty store. And over ssh, only `send` and
`reply` start a store where there's none: anything else says there's no
store there.

Set it up in the usual order: upgrade the relay machine first, then add
`relay = true` (an older `deaddrop` refuses a setting it doesn't know).

## Reading it without a terminal

On your own machine, `deaddrop watch` keeps an **inbox file** of your box,
and runs your **hooks** for each new message. Everything else plugs into
those two: the bundled menu-bar inboxes, notifications, or anything of your
own.

```sh
deaddrop watch                 # check once: the inbox file, then the hooks
deaddrop watch --every 1m      # keep checking, for a machine with no scheduler
deaddrop show ID               # one message in full (--json, --html)
deaddrop open ID               # read it in the browser, and mark it read
deaddrop thread ID             # the whole thread, both sides, in time order
deaddrop with bob              # everything between you and bob
deaddrop open --thread ID      # either one as a page in the browser
deaddrop open --with bob
```

**Threads and conversations** put both sides together: what others sent
you (from your box) and what you sent them (`sent`), oldest first. Each
letter shows who it's from (and as whom), its signature, whether yours have
been read, and its gist, the first real paragraph; the whole letter opens
beneath it. `--json` gives the same to a program (`letters`, each with
`mine`, `gist` and, for yours, `copies`). They mark nothing. Against a store
from before 0.4 they show the other side only, and say so.

Run `deaddrop watch` every minute or so with whatever your machine has:
Crontinental, launchd, systemd, cron, or `--every` in a terminal or a
service. [`examples/watch/`](examples/watch/) has each.

**The inbox file** is `~/.local/state/deaddrop/inbox.json`
(`$XDG_STATE_HOME/deaddrop`, or `$DEADDROP_STATE`). It holds your unread
and read mail, unread first (urgent on top) and newest first, **without the
text**: a viewer fetches that with `deaddrop show` when someone opens a
message, and nothing keeps it.

```json
{
  "version": 1,
  "box": "alice",
  "checked_at": "2026-11-05T23:10:00Z",
  "ok": true,
  "last_ok_at": "2026-11-05T23:10:00Z",
  "unread": 2,
  "urgent": 1,
  "messages": [
    {"id": "2026-11-05T230344-bob-bf2a", "from": "bob", "subject": "The build is down",
     "tags": ["tool-x"], "urgency": "urgent", "state": "unread",
     "sent": "2026-11-05T23:03:44Z", "thread": "", "attachments": ["build.log"]}
  ]
}
```

Every message has every field: `tags` and `attachments` are lists (maybe
empty), `thread` is the id of the message it answers, or `""`. When the
store can't be reached, the file keeps its last list, `ok` is
`false` and `error` says why. Fields are added in later versions, never
renamed. `deaddrop mark` updates the file too, so a viewer shows the change
at once.

**Hooks** are the executables in `~/.config/deaddrop/new.d/` (beside the
config). `watch` runs each once per newly arrived message, with
`DEADDROP_MSG_ID`, `_SENDER`, `_SUBJECT`, `_TAGS`, `_URGENCY`, `_SENT`,
`_BOX` and `_ATTACHMENTS` set, and the message's entry as JSON on stdin:
never the text. What's been announced is remembered, so a restart doesn't
announce everything again, and the very first `watch` announces nothing.
[`examples/new.d/notify`](examples/new.d/notify) is a desktop notification:
Crontinental's `notify` when it's installed (tagged `drop=<id>`, so a
viewer that shows both can fold the notification into the message), else
the system's own.

**Menu-bar inboxes**, both reading the inbox file and calling `show` and
`mark`:

- macOS: [`examples/swiftbar/deaddrop.1m.sh`](examples/swiftbar/), for
  [SwiftBar](https://swiftbar.app). An envelope with the unread count, red
  for urgent; the menu lists your mail with Read (opens it in the browser and
  marks it read), Thread, Mark read or unread, and Archive, and a
  Conversations list opens everything with one person. It's
  `deaddrop bar --format swiftbar`.
- Omarchy: [`examples/omarchy/deaddrop/`](examples/omarchy/deaddrop/), a bar
  widget. Click for your inbox, click a message to read it in the panel
  (that marks it read), archive it or put it back from there, or open its
  thread or the whole conversation in the browser. A switch at the top
  turns it to **Threads**: every conversation, newest first, with chips to
  show only the ones a given speaker was in (a letter's persona, else its
  `persona-<name>` tag, else its box), and chips for each helper's
  pigeonhole once mail comes `--for` them. Copy the folder
  to Omarchy's bar plugins and add it to the bar.
  `omarchy-shell deaddrop open` (or `close`, `toggle`, `check`) opens it on
  the focused monitor: the plugin's service answers once for every bar.

A message is someone else's text, so the pages `show --html` makes escape
everything, show images as their alt text, link only to http, https and
mailto, and forbid the browser from loading anything.

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

The board says "moved" when new mail comes in. By default only counts go to
the board, never subjects or senders, because boards can be on screens
anyone sees.

A box can opt in to saying what's waiting. Name it in `board_detail` in the
store machine's config, and its lookout adds who sent the newest unread
message, when, and its subject (the newest urgent one, when there is one),
on one line, shortened to fit:

```toml
board_detail = ["alice"]
```

| box | lookout, with `board_detail` |
|---|---|
| unread | smoke, "2 unread · newest from bob, 5m ago: Requests for tool X" |
| urgent unread | fire, "1 urgent, 3 unread · urgent from bob, 2h ago: The build is down" |

Other boxes are unchanged. A board on a shared screen can still leave a
mailbox out with the Firetower's own `hide`.

A
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
default, and like the board it names only the box, unless the box is in
`board_detail`: then it says who the message is from and its subject too.

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
              [--attach FILE]... [--follow-up 2d|TIME] [--from automation] [--as NAME] [--for HELPER]...
              [--reply-box BOX] < body.md
deaddrop reply ID [--urgent|--fyi] [--tag T]... [--untag T]... [--as NAME] [--for HELPER]... [--reply-box BOX] [--follow-up 2d|TIME] < body.md
deaddrop inbox [WHO] [--tag T]... [--from WHO]... [--since …] [--read|--all] [--json]
deaddrop pull  [WHO] [--tag T]... [--from WHO]... [--since …] [--unread|--all] [--json]
deaddrop show ID [--json|--html [--fragment]] [--box WHO]
deaddrop sent [--to WHO]... [--tag T]... [--since …] [--json] [--box WHO]
deaddrop key
deaddrop file ID [NAME] [--box WHO]
deaddrop mark read|unread|archive ID... | --tag T... [--from WHO]... [--since …] [--box WHO] [--for H]
deaddrop status [--json]
deaddrop firetower [--next 6m|TIME|none] [--box WHO]...
deaddrop gc [--days 30]
deaddrop watch [--every 1m] [--no-hooks]
deaddrop open ID [--keep-unread] | --thread ID | --with WHO
deaddrop thread ID [--for H] [--json|--html]
deaddrop threads [--for H] [--since …] [--limit N] [--letters [--bodies]] [--json]
deaddrop with WHO [--tag T]... [--for H] [--json|--html]
deaddrop bar [--format text|swiftbar]
deaddrop upgrade [--check] | version | help
```

## Not here (yet)

Real email, a web inbox served from the store, boxes for people outside the team, permissions per
box (every box is readable by anyone who can read the store), search beyond
tags, sender and date, encryption at rest.

## License

MIT
