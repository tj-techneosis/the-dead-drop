import QtQuick
import Quickshell
import Quickshell.Io

// The Dead Drop's one IPC handler, loaded once however many bars there are:
//
//   omarchy-shell deaddrop open | close | toggle | check
//
// Each bar widget copy would otherwise register its own handler, and only the
// first one registered (on whichever monitor) would ever answer. So this
// service answers, and passes open/close/toggle on to the widgets by writing
// the request to a small file beside the inbox file; every copy watches it,
// and only the copy on the focused monitor acts.
Scope {
  id: service

  readonly property string home: Quickshell.env("HOME")
  readonly property string deaddrop: home + "/.local/bin/deaddrop"
  readonly property string stateDir: (Quickshell.env("DEADDROP_STATE") || "")
    || ((Quickshell.env("XDG_STATE_HOME") || (home + "/.local/state")) + "/deaddrop")
  readonly property string requestFile: stateDir + "/panel-request"

  function ask(action) {
    // "<action> <ms>": the time makes every request a change, even a repeat.
    writer.command = ["sh", "-c", 'mkdir -p "$1" && printf "%s %s\\n" "$2" "$3" > "$1/panel-request"',
      "sh", service.stateDir, action, String(Date.now())]
    writer.running = true
  }

  Process { id: writer }
  Process { id: checker; command: [service.deaddrop, "watch", "--no-hooks"] }

  IpcHandler {
    target: "deaddrop"
    function open(): void { service.ask("open") }
    function close(): void { service.ask("close") }
    function toggle(): void { service.ask("toggle") }
    function check(): void { checker.running = true }
  }
}
