# Unread mail in a Claude Code session

`unread-mail` is a Claude Code hook. When the session's box has unread mail,
the model is told the count as context the next time it acts: when someone
sends it a prompt, or after a tool call. It types nothing, starts no turn
and never says a subject or a sender, only the count.

The box is the session's own: `$DEADDROP_BOX`, else `$DEADDROP_REPLY_BOX`
(an assistant with a box of its own), else yours. It repeats itself at most
every five minutes (`DEADDROP_HOOK_QUIET`, in seconds), and at once when the
count goes up.

```sh
mkdir -p ~/.claude/hooks
cp unread-mail ~/.claude/hooks/ && chmod +x ~/.claude/hooks/unread-mail
```

Then in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "~/.claude/hooks/unread-mail", "timeout": 10 } ] }
    ],
    "PostToolUse": [
      { "matcher": "*", "hooks": [ { "type": "command", "command": "~/.claude/hooks/unread-mail", "timeout": 10 } ] }
    ]
  }
}
```

Try it: `echo '{"hook_event_name":"UserPromptSubmit"}' | ~/.claude/hooks/unread-mail`
prints the line when there's unread mail, and nothing when there isn't.

It reads counts with `deaddrop status`, which is quick over a reused ssh
connection. With the store on another machine, each check is one round trip.
