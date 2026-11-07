# Unread mail in a Claude Code session

`unread-mail` is a Claude Code hook. When the session's box has unread mail,
the model is told the count as context the next time it acts: when someone
sends it a prompt, or after a tool call. It types nothing, starts no turn
and never says a subject or a sender, only the count.

The box is the session's own: `$DEADDROP_BOX`, else `$DEADDROP_REPLY_BOX`
(an assistant with a box of its own), else yours. It speaks at once when the
unread or urgent count rises, and otherwise at most every five minutes
(`DEADDROP_HOOK_QUIET`, in seconds), with a small memo per session.

Nothing it says comes from a letter. The counts are two bare numbers from
`deaddrop status --box <box> --count` (0.7.3 or later on the store's
machine; with an older store it stays quiet), the event is its own argument,
and the memo is read only if it holds digits.

```sh
mkdir -p ~/.claude/hooks
cp unread-mail ~/.claude/hooks/ && chmod +x ~/.claude/hooks/unread-mail
```

Then in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "~/.claude/hooks/unread-mail UserPromptSubmit", "timeout": 10 } ] }
    ],
    "PostToolUse": [
      { "matcher": "*", "hooks": [ { "type": "command", "command": "~/.claude/hooks/unread-mail PostToolUse", "timeout": 10 } ] }
    ]
  }
}
```

Try it: `echo '{}' | ~/.claude/hooks/unread-mail UserPromptSubmit`
prints the line when there's unread mail, and nothing when there isn't.

It reads counts with `deaddrop status`, which is quick over a reused ssh
connection. With the store on another machine, each check is one round trip.
