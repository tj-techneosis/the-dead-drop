# The Dead Drop: the design

Why each piece is the way it is. The README is how to use it.

Like a spy's dead drop: you leave something at a known spot, and someone
collects it later, never at the same moment.

## Why

People working with Claude sessions keep passing reports by hand: field
notes from a test run, requests for the next version of a tool, a status
write-up. Each one is copied out of one session and pasted into another.
There's no place they wait, no way to find "everything about tool X that
came in since I last looked", and nothing that says "you have mail".

The same people run jobs that report on things (a tool's numbers after
every run, a daily summary of something they watch), and those reports
have nowhere to go either.

## The stories it serves

1. **Send a write-up.** "Write up our report and our requests for the next
   version of tool X and send it to Bob." The session writes Markdown and
   sends it: to `bob`, tags `tool-x`, `request`.
2. **Send field notes.** "Ran tool Y over the weekend: send the suggestions
   to Alice as field notes." To `alice`, tags `tool-y`, `field-notes`.
3. **People set up their own reports.** Someone tuning tool Y books a job
   that, after every run, files that run's numbers in their own box, from
   `automation`, tagged `tool-y` and `report`. The Dead Drop just takes the
   document; what gets reported, and when, is theirs.
4. **Pull it all into a session.** "Look at all the automated reports from
   tool Y and any notes from Bob about it." One command gathers every
   matching message, unread first, into the session, which works from it,
   then marks them read or archives them.
5. **See at a glance that something is waiting.** The Firetower shows each
   box as clear, smoke or fire, and the subject never leaves the box.

## Files, not a database

A message is a Markdown file with a small YAML header; its state is which
folder it's in (`new`, `read`, `archive`). Moving it is the only change of
state. That means:

- a person can read the store with `ls` and `cat`, and back it up with
  anything;
- there's no index to get out of step, and no migration;
- every write is a temp file renamed into place, so nobody sees half a
  message; a message is renamed into `new/` only after its attachments are
  in place.

The header is written one way and read only as written: the subject is a
JSON string (which YAML reads as a double-quoted scalar), tags are a flow
list of words that need no quoting, times are RFC 3339 in UTC. The path is
the truth: a file whose `id` disagrees with its name is read as its name.

**Ids** are `<sent, to the second, UTC>-<sender>-<4 hex>`: they sort by
time and say who sent them, so a listing or a `mark` line is readable
without opening anything.

**A lock per box.** Renames are atomic by themselves, but a follow-up
rewrites a message in `new/` to raise it, and a `mark` might move it at the
same moment; without a lock the rewrite could put back a message that had
just been read. Every change of state takes the box's `flock`.

## Names

A box is any lowercase name, and exists once something has been sent to it:
no sign-up, no list of members. `me` means the caller. `automation` is a
sender and never a box, so a job's reports always land with a person.

Who the caller is comes from `$DEADDROP_FROM`, then `me` in the config, then
the login name. On a shared machine everyone may log in as the same user,
so in remote mode the login name is never used: the caller's name must be
set, and it is sent across explicitly. On that machine itself,
`shared_login = true` turns the login-name fallback off too, so a session
there without `DEADDROP_FROM` is refused rather than filed as the shared
user.

You send as yourself or as `automation`, never as another person: `--from`
takes nothing else. It isn't security (anyone can set `DEADDROP_FROM`), but
it stops a slip from putting a message in someone's name.

There are no permissions per box in this version. Anyone who can read the
store can read every box. That's why **secrets never go in a message**: the
store's own README says so, the skill says so, and `send` refuses the shapes
that are never anything but a secret (private key blocks, cloud and API
token prefixes with their full length). It's a backstop for slips, not a
scanner; a document that talks *about* tokens goes through.

## Pull, then mark

`pull` prints everything that matches, in full, in one go: unread first,
oldest first within that, so a session reads mail in the order it arrived
and sees the new before the old. Each message sits between separator lines
that carry a random mark chosen per pull, so a document can't end its own
message early by quoting a separator.

`pull` never marks anything. A session that crashes after pulling loses no
mail; the next one pulls the same. Marking is a separate, explicit step
(`mark read|archive`), by the ids `pull` printed or by the same tags and
senders. `read` mail is still pulled (it's been seen, not dealt with);
`archive` is filed and out of the way, and goes after 30 days (`gc`,
counted from when it was filed).

Filters: several `--tag` all have to match (a tool's reports are `--tag
tool-y --tag report`); several `--from` match any (`--from automation --from
bob`). That covers story 4 in one command.

## Remote mode

The store lives on one machine. Everywhere else, every command runs there
over ssh (`host` in the config): bodies and attachments go over stdin (a
send with attachments sends one tar stream: the body, then the files), and
output comes back on stdout. Nothing is written on the caller's side: not a
cache, not a temp file. The one exception is the person's own choice: the
inbox file `deaddrop watch` keeps (below), which holds no message text. ssh runs in batch mode, so a missing key fails
instead of waiting for a password a session can't type.

The far side is told `DEADDROP_FROM` (the caller) and `DEADDROP_HOPS` (how
many machines the command has come through). It never sends the command round again
on its own: if its config has a `host` and no `relay = true`, it isn't the
store, and it refuses with a message saying so rather than answering from an
empty store. With `relay = true`, it passes the
command on to its own host, as the same caller, with an attachment stream
passed through as it is. That's for someone who can only reach the store
through another machine.

A relay passes the caller's name on, as every remote command does: the
store takes `DEADDROP_FROM` on trust, from anyone who can ssh in. A relay
adds no one: whoever can run a command on the relay machine already has its
key to the store. Names in the Dead Drop stop slips, not people (see
Names); a store that has to tell people apart needs a login each. Three hops is the limit, so a relay pointed at
itself fails with a message rather than looping. Over ssh, only `send` and
`reply` may start a store where there's none; every other command says
there's no store, so a wrong `host` shows as an error, never as an empty
inbox. Every argument is
single-quoted for the far shell.

## Your own machine: the inbox file and hooks

Mail that only a terminal can show isn't mail you read. So the Dead Drop
provides the wiring, and viewers plug into it, the way the Firetower ships a
feed and boards that read it, and anyone can put their own look on top:

- **`deaddrop watch`** reads the caller's box (over ssh, like any command)
  and writes an **inbox file** on their own machine: every unread and read
  message's header, never its text. The format is documented and versioned,
  because other people's viewers read it. When the store can't be reached
  the file keeps its last list and says `ok: false`, so a viewer shows stale
  mail as stale rather than as no mail.
- **`deaddrop show ID`** hands out one message's text, on demand. Nothing
  caches it: the inbox file has headers only, and the pages `open` writes for
  the browser are removed within the hour.
- **`new.d` hooks**, like Crontinental's `notify.d`: executables run once per
  newly arrived message, given its header (never its text). What's been
  announced is written down before any hook runs, so a hook that hangs or a
  pass that dies never announces the same mail twice; the first pass
  announces nothing, since it would be everything at once. Mail read before
  a pass sees it is not news.
- **Viewers**: a SwiftBar menu (`deaddrop bar --format swiftbar`, so the
  menu is tested with the rest) and an Omarchy bar widget. Both read the
  file and call `show` and `mark`; `mark` rewrites the file at once, so a
  click shows. Someone else's viewer does the same.

A message is someone else's text, so the HTML that `show --html` makes is
built from escaped text with a fixed set of tags, links only to http, https
and mailto, shows images as their alt text, and carries a Content Security
Policy that loads nothing. The Omarchy widget shows the same HTML rather
than Qt's own Markdown, which would render raw HTML and fetch images.

## No clock

The Dead Drop runs nothing by itself, like its siblings, with one exception
for people with no scheduler: `deaddrop watch --every 1m` loops (a pass,
then sleep, no faster than every 10 seconds). Everyone with a scheduler
books `deaddrop watch` with it instead; `examples/watch/` has Crontinental,
launchd, systemd and cron.

- **The board** is a scheduled `deaddrop firetower` on the store's machine:
  one lookout per box, `mail/<who>/inbox`, clear, smoke or fire. Its value
  stands for the newest unread message, so the board says "moved" when mail
  arrives. The value is a hash of the id, not the id: ids name their
  sender, and by default nothing about a message (subject, sender) goes to
  a board, which may be on a screen anyone sees. A box named in
  `board_detail` opts in: its line adds the sender, age and subject of the
  newest unread message (the newest urgent one first), so its owner knows
  what's waiting without opening anything. It's per box because it's that
  person's call, and the Firetower's own `hide` keeps a mailbox off a
  board that shouldn't show it. The line is shortened here, on a word, to
  stay inside the Firetower's 200-character cut, and the label never names
  a sender. Under Crontinental it promises its
  next report from `$CRONTINENTAL_NEXT`, so a stopped job shows as "no
  word".
- **Follow-ups** are one-off Crontinental jobs booked at send time, named
  after the message, that run the installed `deaddrop` (whose path survives
  upgrades, unlike a release's folder) as `deaddrop follow-up BOX ID`: raise it to
  urgent if it's still unread, else nothing. Marking the message read or
  archived cancels the job by its name; if the cancel misses, the job finds
  the message read and does nothing. If Crontinental isn't there, a send
  with `--follow-up` is refused before anything is written; if booking
  fails after the message is delivered, it says so and doesn't fail the
  send (sending again would only make two).
- **The ring**, off by default, is `crontinental notify --urgent` when
  urgent mail arrives or a follow-up raises one. Like the board, it names
  the box and nothing else, unless the box is in `board_detail`.

## Reports people set up

Not part of the tool. A report is any job that ends in `deaddrop send --to
<owner> --from automation --tag <tool> --tag report`. The skill shows the
pattern, so a session told "file a report of every run of tool Y in my box"
can set it up; the job lives with whatever sends it.

## Out of scope for now

Real email, a web inbox served from the store, boxes for people outside the team, permissions per
box (per-person users on the store's machine would give each box its own
owner), search beyond tags, sender and date, encryption at rest.
