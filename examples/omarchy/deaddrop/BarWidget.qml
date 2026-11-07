import QtQuick
import QtQuick.Controls
import QtQuick.Layouts
import Quickshell
import Quickshell.Io
import Quickshell.Hyprland
import qs.Commons
import qs.Ui

// The Dead Drop in Omarchy's bar (Omarchy 4, whose bar is a Quickshell shell).
//
// It reads the inbox file `deaddrop watch` keeps (~/.local/state/deaddrop/
// inbox.json, or $XDG_STATE_HOME/deaddrop) and redraws when the file changes.
// It never asks the store itself, except through the deaddrop CLI:
//
//   deaddrop show ID --html --fragment   a message's text, when it's opened
//   deaddrop mark read|unread|archive ID (which rewrites the inbox file)
//   deaddrop watch --no-hooks            "check now"
//
// Service.qml answers `omarchy-shell deaddrop open|close|toggle|check` once,
// for every bar, and the copy on the focused monitor opens.
//
// Two views, switched at the top of the panel: Inbox (the person's mail, or
// one helper's pigeonhole) and Threads (`deaddrop threads`: every
// conversation, newest first, narrowed to one speaker with a chip; a row
// opens the whole thread in the browser). A speaker is a letter's sealed
// persona, else its persona-<name> tag, else its box.
//
// Something has to run `deaddrop watch` (Crontinental, a systemd timer, or
// `deaddrop watch --every 1m` as a service): see the Dead Drop's README.
//
// The bar: an envelope, hidden-dim with no mail, a count when there's unread
// mail, red when some of it is urgent. Click for the inbox: every message,
// unread first. Click one to read it (that marks it read); archive it or put
// it back from there. Right click checks now.
//
// A message is someone else's text. The panel shows the HTML the CLI makes
// from it, which is escaped and holds no images, and opens only http(s) and
// mailto links, in the browser.
//
// A theme can set its own colours in a deaddrop.toml beside its colors.toml:
//
//   urgent = "#fd6883"
//   unread = "#f9cc6c"
Panel {
  id: root
  moduleName: "deaddrop"
  // `omarchy-shell deaddrop ...` is answered once, by Service.qml, which
  // writes the request to panel-request; the copy on the focused monitor
  // acts on it. So no copy registers a handler of its own.
  ipcTarget: ""
  readonly property string screenName: QsWindow.window && QsWindow.window.screen ? String(QsWindow.window.screen.name) : ""
  readonly property bool onFocusedScreen: screenName === "" || !Hyprland.focusedMonitor
    || String(Hyprland.focusedMonitor.name) === screenName
  property string lastRequest: ""   // the request already seen; "" until the first read

  function takeRequest(raw) {
    var line = String(raw || "").trim()
    var first = root.lastRequest === ""
    if (line === "" || line === root.lastRequest) return
    root.lastRequest = line
    if (first) return  // one left from before this copy started: not for us
    if (!root.onFocusedScreen) return
    var action = line.split(" ")[0]
    if (action === "open") root.open()
    else if (action === "close") root.close()
    else if (action === "toggle") root.toggle()
  }

  readonly property string home: Quickshell.env("HOME")
  readonly property string deaddrop: home + "/.local/bin/deaddrop"
  readonly property string stateDir: (Quickshell.env("DEADDROP_STATE") || "")
    || ((Quickshell.env("XDG_STATE_HOME") || (home + "/.local/state")) + "/deaddrop")
  readonly property string themeDir: home + "/.local/state/omarchy/current/theme"

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color dim: Qt.darker(foreground, 1.55)
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family

  property var palette: ({})
  property var overrides: ({})
  readonly property color urgentColor: overrides.urgent || (bar ? bar.urgent : Color.urgent)
  readonly property color unreadColor: overrides.unread || palette.yellow || "#e0b040"

  // The inbox file.
  property bool watched: false   // there is an inbox file at all
  property bool ok: true
  property string problem: ""
  property string checkedAt: ""
  property int unread: 0
  property int urgent: 0
  property var messages: []

  // The reader: the message open in the panel, if any.
  property var reading: null
  property string readingHtml: ""
  property string readingNote: ""
  property string actionNote: ""

  // Views: "inbox" or "threads"; the pigeonhole shown ("" is the person's).
  property string view: "inbox"
  property string hole: ""
  property var holes: []          // helpers with mail in this box
  property var holeMessages: []   // a helper's pigeonhole, fetched (no inbox file for it)
  property var topics: []         // threads, each with .speakers
  property var speakers: []       // every speaker in them, busiest first
  property string who: ""         // threads: only those this speaker is in
  property string threadsNote: ""
  property string threadsErr: ""
  property string holeNote: ""       // a helper's pigeonhole: loading, or what went wrong
  property string listerNote: ""     // finding the pigeonholes went wrong
  property bool holeStale: false     // a fetch came back for another pigeonhole
  property bool threadsStale: false
  property var holeUnread: ({})      // helper -> unread count, for the chips
  property var pending: []           // actions waiting for the one running
  readonly property int threadLimit: 80
  readonly property var shown: hole === "" ? messages : holeMessages
  readonly property var shownTopics: who === "" ? topics : topics.filter(function(t) { return t.speakers.indexOf(root.who) >= 0 })

  property int cursor: 0
  property bool cursorActive: false
  property int tick: 0  // a minute passing: ages are worked out again

  readonly property bool hasUrgent: urgent > 0
  readonly property bool hasUnread: unread > 0

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  function colorsIn(raw) {
    var out = {}
    var lines = String(raw || "").split("\n")
    for (var i = 0; i < lines.length; i++) {
      var m = lines[i].match(/^\s*([A-Za-z0-9_-]+)\s*=\s*["']?(#[0-9A-Fa-f]{6})/)
      if (m) out[m[1]] = m[2]
    }
    return out
  }

  function ago(iso) {
    root.tick
    var t = Date.parse(iso)
    if (isNaN(t)) return ""
    var s = Math.max(0, (Date.now() - t) / 1000)
    if (s < 60) return "just now"
    if (s < 3600) return Math.floor(s / 60) + "m ago"
    if (s < 172800) return Math.floor(s / 3600) + "h ago"
    return Math.floor(s / 86400) + "d ago"
  }

  // Who it's from, as far as this machine can tell (deaddrop checks the
  // signature here, against this machine's list of keys).
  function sender(m) {
    var s = String(m.from || "") + (m.persona ? ", as " + m.persona : "")
    if (m.signature === "bad") return s + " · forged or changed"
    if (m.signature === "unknown") return s + " · can't check"
    if (m.signature !== "verified") return s + " · unsigned"
    return s
  }

  // Who spoke a letter: its sealed persona, else a persona-<name> tag, else
  // its box. ("persona-release-manager" reads "Release manager".)
  // Only a verified letter's persona or tag says who spoke: anyone can write
  // either onto a letter that doesn't verify.
  function speakerOf(l, box) {
    if (l.signature !== "verified") return l.mine ? box : String(l.from || "")
    if (l.persona) return String(l.persona)
    var tags = l.tags || []
    for (var i = 0; i < tags.length; i++) {
      var t = String(tags[i])
      if (t.indexOf("persona-") === 0 && t.length > 8) {
        var w = t.slice(8).replace(/-/g, " ")
        return w.charAt(0).toUpperCase() + w.slice(1)
      }
    }
    return l.mine ? box : String(l.from || "")
  }

  function loadTopics(raw) {
    if (threadLister.askedFor !== root.hole) { root.threadsStale = true; return } // for another pigeonhole: asked again on exit
    var data
    try { data = JSON.parse(raw) } catch (e) { root.topics = []; root.speakers = []; root.who = ""; root.threadsNote = "Couldn't read the threads."; return }
    root.threadsErr = ""
    var notes = []
    if (data.sent_missing) notes.push(String(data.sent_missing))
    if (root.who === "" && (data.topics || []).length >= root.threadLimit) notes.push("The newest " + root.threadLimit + " threads; pick someone to see all of theirs.")
    root.threadsNote = notes.join(" ")
    var count = {}
    var list = data.topics || []
    for (var i = 0; i < list.length; i++) {
      var t = list[i]
      var seen = []
      var letters = t.letters || []
      for (var j = 0; j < letters.length; j++) {
        var s = root.speakerOf(letters[j], String(data.box || ""))
        if (s && seen.indexOf(s) < 0) seen.push(s)
      }
      t.speakers = seen
      for (var k = 0; k < seen.length; k++) count[seen[k]] = (count[seen[k]] || 0) + 1
    }
    root.topics = list
    root.clampCursor()
  }

  // Everyone in the mail, from `deaddrop people`: the dropdown lists them
  // all, newest first, not only those in the threads on screen.
  function loadPeople(raw) {
    var list
    try { list = JSON.parse(raw) } catch (e) { return }
    var names = []
    for (var i = 0; i < list.length; i++) {
      var n = list[i].persona ? String(list[i].persona) : String(list[i].box)
      if (names.indexOf(n) < 0) names.push(n)
    }
    root.speakers = names
    if (root.who !== "" && names.indexOf(root.who) < 0) root.who = ""
  }

  function loadHoles(raw) {
    var list
    try { list = JSON.parse(raw) } catch (e) { return }
    var hs = []
    var unread = {}
    for (var i = 0; i < list.length; i++) {
      var h = list[i].pigeonhole
      if (!h) continue
      h = String(h)
      if (hs.indexOf(h) < 0) hs.push(h)
      if (list[i].state === "unread") unread[h] = (unread[h] || 0) + 1
    }
    // The pigeonhole you picked stays, even with everything in it filed.
    if (root.hole !== "" && hs.indexOf(root.hole) < 0) hs.push(root.hole)
    hs.sort()
    root.holes = hs
    root.holeUnread = unread
  }

  function loadHole(raw) {
    if (holeReader.askedFor !== root.hole) { root.holeStale = true; return } // for another pigeonhole: asked again on exit
    var list
    try { list = JSON.parse(raw) } catch (e) { root.holeNote = root.holeNote || "Couldn't read " + root.hole + "'s pigeonhole."; return }
    root.holeNote = ""
    // Unread first (urgent on top), then read, newest first: as the inbox file.
    function rank(m) { return m.state === "unread" ? (m.urgency === "urgent" ? 0 : 1) : 2 }
    list.sort(function(a, b) { return rank(a) - rank(b) || String(b.sent).localeCompare(String(a.sent)) })
    root.holeMessages = list
    root.clampCursor()
  }

  function clampCursor() {
    var n = root.view === "threads" ? root.shownTopics.length : root.shown.length
    if (root.cursor > n - 1) root.cursor = Math.max(0, n - 1)
  }

  function forArgs() { return root.hole === "" ? [] : ["--for", root.hole] }

  // Each fetch remembers which pigeonhole it was for; one that finishes after
  // the chip changed is thrown away and asked again (see loadHole, loadTopics).
  function refreshViews() {
    if (!holeLister.running) holeLister.running = true
    if (root.hole !== "" && !holeReader.running) {
      holeReader.askedFor = root.hole
      holeReader.command = [root.deaddrop, "inbox", "--read", "--json"].concat(root.forArgs())
      if (root.holeMessages.length === 0) root.holeNote = "Looking in " + root.hole + "'s pigeonhole…"
      holeReader.running = true
    }
    if (root.view === "threads" && !peopleLister.running) {
      peopleLister.command = [root.deaddrop, "people", "--json"].concat(root.forArgs())
      peopleLister.running = true
    }
    if (root.view === "threads" && !threadLister.running) {
      threadLister.askedFor = root.hole
      root.threadsErr = ""
      // One speaker picked: all their threads, however old. Otherwise the newest.
      var limit = root.who === "" ? root.threadLimit : 1000
      threadLister.command = [root.deaddrop, "threads", "--json", "--letters", "--limit", String(limit)].concat(root.forArgs())
      threadLister.running = true
    }
  }

  function pickView(v) { root.view = v; root.cursor = 0; root.refreshViews() }
  function pickHole(h) { root.hole = h; root.holeMessages = []; root.holeNote = ""; root.topics = []; root.speakers = []; root.who = ""; root.threadsErr = ""; root.cursor = 0; root.refreshViews() }
  function openTopic(t) { root.runAction(["open", "--thread", String(t.root)].concat(root.forArgs()), "Thread") }

  // The pigeonhole on show, as a line: a helper's counts come from its fetch.
  function holeLine() {
    if (holeReader.running && root.holeMessages.length === 0) return "For " + root.hole + ": looking…"
    if (root.holeNote !== "" && root.holeMessages.length === 0) return "For " + root.hole + ": couldn't read it"
    var n = 0
    for (var i = 0; i < root.holeMessages.length; i++) if (root.holeMessages[i].state === "unread") n++
    return "For " + root.hole + ": " + (n > 0 ? n + " unread" : "no unread mail")
  }

  function headline() {
    if (!root.watched) return "Not watching yet"
    if (root.urgent > 0)
      return root.urgent + " urgent" + (root.unread > root.urgent ? ", " + root.unread + " unread" : "")
    if (root.unread > 0) return root.unread + " unread"
    return "No mail"
  }

  function loadInbox(raw) {
    var data
    try { data = JSON.parse(raw) } catch (e) { root.watched = false; return }
    root.watched = true
    root.ok = data.ok !== false
    root.problem = data.error ? String(data.error) : ""
    root.checkedAt = String(data.checked_at || "")
    root.unread = Number(data.unread || 0)
    root.urgent = Number(data.urgent || 0)
    root.messages = data.messages || []
    root.clampCursor()
    // The open message, as the file now has it (or gone, once archived).
    if (root.reading) {
      var still = null
      for (var i = 0; i < root.messages.length; i++)
        if (root.messages[i].id === root.reading.id) still = root.messages[i]
      if (still) root.reading = still
    }
  }

  function tooltipLines() {
    if (!root.watched) return "Dead Drop: not watching yet (deaddrop watch)"
    var lines = ["Dead Drop: " + root.headline().toLowerCase()]
    if (!root.ok) lines.push("Can't reach the store: " + root.problem)
    var n = 0
    for (var i = 0; i < root.messages.length && n < 5; i++) {
      var m = root.messages[i]
      if (m.state !== "unread") continue
      lines.push(m.from + ": " + m.subject)
      n++
    }
    return lines.join("\n")
  }

  // ---- actions, one at a time ----

  // One at a time; one asked for while another runs waits its turn.
  function runAction(cmd, note) {
    if (actor.running) {
      // A check asked for again while one runs or waits is the same check;
      // anything else (a mark, an open) always waits its turn.
      if (cmd[0] === "watch") {
        var key = cmd.join(" ")
        if (actor.command.slice(1).join(" ") === key) return
        for (var i = 0; i < root.pending.length; i++) if (root.pending[i][0].join(" ") === key) return
      }
      root.pending = root.pending.concat([[cmd, note]])
      return
    }
    actor.label = note
    actor.command = [root.deaddrop].concat(cmd)
    actor.running = true
  }

  function checkNow() { root.runAction(["watch", "--no-hooks"], "Check") }

  // The text that comes back is for the message asked for; one opened while
  // another's text is still on its way is fetched once that one's done.
  function openMessage(m) {
    root.reading = m
    root.readingHtml = ""
    root.readingNote = "Opening…"
    if (reader.running) { reader.next = String(m.id); return }
    reader.askedFor = String(m.id)
    reader.command = [root.deaddrop, "show", String(m.id), "--html", "--fragment"].concat(root.forArgs())
    reader.running = true
  }

  function closeMessage() {
    root.reading = null
    root.readingHtml = ""
    root.readingNote = ""
  }

  function archiveMessage(m) { root.runAction(["mark", "archive", String(m.id)].concat(root.forArgs()), "Archive"); root.closeMessage() }
  function putBackMessage(m) { root.runAction(["mark", "unread", String(m.id)].concat(root.forArgs()), "Mark unread"); root.closeMessage() }
  function markMessageRead(m) { root.runAction(["mark", "read", String(m.id)].concat(root.forArgs()), "Mark read") }

  function openLink(link) {
    var l = String(link).toLowerCase()
    if (l.indexOf("https://") === 0 || l.indexOf("http://") === 0 || l.indexOf("mailto:") === 0)
      Qt.openUrlExternally(link)
  }

  function moveCursor(dy) {
    var n = root.view === "threads" ? root.shownTopics.length : root.shown.length
    root.cursor = Math.max(0, Math.min(n - 1, root.cursor + dy))
  }

  onOpenedChanged: if (opened) {
    cursorActive = false
    cursor = 0
    actionNote = ""
    closeMessage()
    refreshViews()
  }

  FileView {
    id: inboxFile
    path: root.stateDir + "/inbox.json"
    watchChanges: true
    printErrors: false
    onFileChanged: reload()
    onLoaded: root.loadInbox(text())
    onLoadFailed: root.watched = false
  }

  FileView {
    id: requestFile
    path: root.stateDir + "/panel-request"
    watchChanges: true
    printErrors: false
    onFileChanged: reload()
    onLoaded: root.takeRequest(text())
    onLoadFailed: { if (root.lastRequest === "") root.lastRequest = "none"; retryRequest.start() }
  }

  // The file appears with the first request; until then, look again now and
  // then, since a file that isn't there can't be watched.
  Timer {
    id: retryRequest
    interval: 2000
    onTriggered: requestFile.reload()
  }

  FileView {
    path: root.themeDir + "/colors.toml"
    watchChanges: true
    printErrors: false
    onFileChanged: reload()
    onLoaded: root.palette = root.colorsIn(text())
  }

  FileView {
    path: root.themeDir + "/deaddrop.toml"
    watchChanges: true
    printErrors: false
    onFileChanged: reload()
    onLoaded: root.overrides = root.colorsIn(text())
    onLoadFailed: root.overrides = ({})
  }

  // Reading a message: its text, then it's marked read (unless it is).
  Process {
    id: reader
    property string askedFor: ""
    property string next: ""
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (root.reading && String(root.reading.id) === reader.askedFor) root.readingHtml = text
    }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (text.trim() !== "" && root.reading && String(root.reading.id) === reader.askedFor) root.readingNote = text.trim().replace(/^deaddrop: /, "")
    }
    onExited: function(exitCode) {
      var asked = reader.askedFor
      if (reader.next !== "") {
        // Another message was opened meanwhile: fetch that one now.
        var id = reader.next
        reader.next = ""
        reader.askedFor = id
        reader.command = [root.deaddrop, "show", id, "--html", "--fragment"].concat(root.forArgs())
        reader.running = true
        return
      }
      if (!root.reading || String(root.reading.id) !== asked) return
      if (exitCode !== 0) {
        if (root.readingNote === "Opening…") root.readingNote = "Couldn't open it (deaddrop show exited " + exitCode + ")."
        return
      }
      root.readingNote = ""
      if (root.reading.state === "unread") root.markMessageRead(root.reading)
    }
  }

  Process {
    id: holeLister
    command: [root.deaddrop, "inbox", "--for", "*", "--all", "--json"]
    stdout: StdioCollector { waitForEnd: true; onStreamFinished: root.loadHoles(text) }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: root.listerNote = text.trim() !== "" ? "Pigeonholes: " + text.trim().replace(/^deaddrop: /, "") : ""
    }
  }

  Process {
    id: peopleLister
    stdout: StdioCollector { waitForEnd: true; onStreamFinished: root.loadPeople(text) }
  }

  Process {
    id: holeReader
    property string askedFor: ""
    onExited: function(exitCode) {
      if (exitCode !== 0 && holeReader.askedFor === root.hole && root.holeNote.indexOf("Looking") === 0) root.holeNote = "Couldn't read " + root.hole + "'s pigeonhole."
      if (root.holeStale || holeReader.askedFor !== root.hole) { root.holeStale = false; Qt.callLater(root.refreshViews) }
    }
    stdout: StdioCollector { waitForEnd: true; onStreamFinished: root.loadHole(text) }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (text.trim() !== "" && holeReader.askedFor === root.hole) root.holeNote = text.trim().replace(/^deaddrop: /, "")
    }
  }

  Process {
    id: threadLister
    property string askedFor: ""
    onExited: function(exitCode) { if (root.threadsStale || threadLister.askedFor !== root.hole) { root.threadsStale = false; Qt.callLater(root.refreshViews) } }
    stdout: StdioCollector { waitForEnd: true; onStreamFinished: root.loadTopics(text) }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (text.trim() !== "" && threadLister.askedFor === root.hole) root.threadsErr = text.trim().replace(/^deaddrop: /, "")
    }
  }

  Process {
    id: actor
    property string label: ""
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (text.trim() !== "") root.actionNote = actor.label + " didn't work: " + text.trim().replace(/^deaddrop: /, "")
    }
    // mark and watch rewrite the inbox file; the FileView picks that up. A
    // helper's pigeonhole and the threads are fetched again.
    onExited: function(exitCode) {
      // The same action working now clears what it said when it didn't.
      if (exitCode === 0 && root.actionNote.indexOf(actor.label + " didn't work") === 0) root.actionNote = ""
      inboxFile.reload()
      root.refreshViews()
      if (root.pending.length > 0) {
        var next = root.pending[0]
        root.pending = root.pending.slice(1)
        Qt.callLater(function() { root.runAction(next[0], next[1]) })
      }
    }
  }

  // Ages on screen move on even when the file doesn't.
  Timer {
    interval: 60000
    running: true
    repeat: true
    onTriggered: root.tick++
  }

  WidgetButton {
    id: button
    anchors.fill: parent
    bar: root.bar

    text: ""
    labelVisible: false
    hasVisualContent: true
    fixedWidth: icon.implicitWidth + button.scaledHorizontalMargin * 2
    active: root.hasUnread || (root.watched && !root.ok)
    activeColor: root.hasUrgent ? root.urgentColor : root.unreadColor

    tooltipText: root.opened ? "" : root.tooltipLines()

    onPressed: function(b) {
      if (b === Qt.RightButton) root.checkNow()
      else root.toggle()
    }

    Row {
      id: icon
      anchors.centerIn: parent
      spacing: Style.space(4)
      readonly property color ink: button.active ? button.activeColor : (root.watched ? button.foreground : root.dim)

      Text {
        textFormat: Text.PlainText
        anchors.verticalCenter: parent.verticalCenter
        // nf-md-email / nf-md-email_alert
        text: root.hasUrgent ? "󰛏" : "󰇮"
        color: icon.ink
        font.family: button.fontFamily
        font.pixelSize: button.fontSize
        renderType: Text.NativeRendering
      }

      Text {
        visible: root.hasUnread
        textFormat: Text.PlainText
        anchors.verticalCenter: parent.verticalCenter
        text: String(root.unread)
        color: icon.ink
        font.family: button.fontFamily
        font.pixelSize: button.fontSize
        renderType: Text.NativeRendering
      }
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(460))
    contentHeight: panel.fittedContentHeight(column.implicitHeight, Style.space(640))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onMoveRequested: function(dx, dy) {
        if (root.reading) { panelFlick.contentY = Math.max(0, Math.min(panelFlick.contentHeight - panelFlick.height, panelFlick.contentY + dy * Style.space(60))); return }
        if (!root.cursorActive) { root.cursorActive = true; return }
        root.moveCursor(dy)
      }
      onActivateRequested: {
        if (root.reading || !root.cursorActive) return
        if (root.view === "threads") { if (root.shownTopics[root.cursor]) root.openTopic(root.shownTopics[root.cursor]) }
        else if (root.shown[root.cursor]) root.openMessage(root.shown[root.cursor])
      }
      onCloseRequested: if (root.reading) root.closeMessage(); else root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(t) {
        var k = String(t).toLowerCase()
        var m = root.reading || (root.cursorActive && root.view === "inbox" ? root.shown[root.cursor] : null)
        if (k === "v" && !root.reading) root.pickView(root.view === "inbox" ? "threads" : "inbox")
        else if (k === "r") { root.checkNow(); root.refreshViews() }
        else if (k === "a" && m) root.archiveMessage(m)
        else if (k === "u" && m) root.putBackMessage(m)
        else if (k === "b" && root.reading) root.closeMessage()
        else if (k === "t" && m) root.runAction(["open", "--thread", String(m.id)].concat(root.forArgs()), "Thread")
        else if (k === "c" && m && m.from !== "automation") root.runAction(["open", "--with", String(m.from)].concat(root.forArgs()), "Conversation")
      }

      Flickable {
        id: panelFlick
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: panelFlick.width
          spacing: Style.space(12)

          PanelHero {
            width: parent.width
            title: root.reading ? String(root.reading.subject || "") : "Dead Drop"
            meta: root.reading
              ? "From " + root.sender(root.reading) + " · " + root.ago(root.reading.sent) + (root.reading.urgency === "urgent" ? " · urgent" : "")
              : root.hole !== "" ? root.holeLine()
              : root.headline() + (root.watched && root.checkedAt ? " · checked " + root.ago(root.checkedAt) : "")
            foreground: root.foreground
            fontFamily: root.fontFamily
            iconComponent: Component {
              Text {
                text: root.hasUrgent ? "󰛏" : "󰇮"
                color: root.hasUrgent ? root.urgentColor : (root.hasUnread ? root.unreadColor : root.foreground)
                font.family: root.fontFamily
                font.pixelSize: Style.font.display
              }
            }
          }

          // What's wrong, if anything: the store out of reach, an action that failed.
          Text {
            visible: text !== ""
            width: parent.width
            leftPadding: Style.space(10)
            rightPadding: Style.space(10)
            textFormat: Text.PlainText
            wrapMode: Text.WordWrap
            text: root.actionNote !== "" ? root.actionNote
              : !root.watched ? "Nothing is watching your Dead Drop yet. Run deaddrop watch on a timer (see its README), or right click to check once."
              : !root.ok ? "Can't reach the store, so this is the last list: " + root.problem
              : ""
            color: root.actionNote !== "" || !root.ok ? root.urgentColor : root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }

          // ---- what's shown: inbox or threads, whose pigeonhole, which speaker ----

          Text {
            visible: !root.reading && root.listerNote !== ""
            width: parent.width
            leftPadding: Style.space(10)
            rightPadding: Style.space(10)
            textFormat: Text.PlainText
            wrapMode: Text.WordWrap
            text: root.listerNote
            color: root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }

          Flow {
            visible: !root.reading
            width: parent.width
            leftPadding: Style.space(10)
            rightPadding: Style.space(10)
            spacing: Style.space(6)

            Chip { label: "Inbox"; on: root.view === "inbox"; onPicked: root.pickView("inbox") }
            Chip { label: "Threads"; on: root.view === "threads"; onPicked: root.pickView("threads") }
            Item { visible: root.holes.length > 0; width: Style.space(8); height: 1 }
            Chip { visible: root.holes.length > 0; label: "Mine"; on: root.hole === ""; onPicked: root.pickHole("") }
            Repeater {
              model: root.holes
              Chip {
                required property string modelData
                label: (root.holeUnread[modelData] ? "(" + root.holeUnread[modelData] + ") " : "") + "for " + modelData
                on: root.hole === modelData
                onPicked: root.pickHole(modelData)
              }
            }
          }

          // Who: a dropdown of everyone in the mail ("Everyone" first).
          RowLayout {
            visible: !root.reading && root.view === "threads" && root.speakers.length > 0
            width: parent.width
            spacing: Style.space(8)

            Text {
              Layout.leftMargin: Style.space(10)
              textFormat: Text.PlainText
              text: "Who"
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            ComboBox {
              id: whoBox
              Layout.fillWidth: true
              Layout.rightMargin: Style.space(10)
              model: ["Everyone"].concat(root.speakers)
              currentIndex: root.who === "" ? 0 : Math.max(0, root.speakers.indexOf(root.who) + 1)
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              onActivated: function(index) {
                root.who = index <= 0 ? "" : root.speakers[index - 1]
                root.cursor = 0
                root.refreshViews()
              }
            }
          }

          // ---- the threads ----

          Column {
            id: topicColumn
            visible: !root.reading && root.view === "threads"
            width: parent.width
            spacing: Style.space(4)

            Text {
              visible: text !== ""
              width: parent.width
              leftPadding: Style.space(10)
              rightPadding: Style.space(10)
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: root.threadsErr !== "" ? root.threadsErr : root.threadsNote
              color: root.threadsErr !== "" ? root.urgentColor : root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            Repeater {
              model: root.shownTopics
              TopicRow {
                required property var modelData
                required property int index
                width: topicColumn.width
                topic: modelData
                rowIndex: index
              }
            }

            Text {
              visible: root.shownTopics.length === 0 && !threadLister.running && root.threadsErr === ""
              width: parent.width
              leftPadding: Style.space(10)
              textFormat: Text.PlainText
              text: root.who !== "" ? "No threads with " + root.who + "." : "No threads yet."
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
            }
          }

          // ---- the list ----

          Column {
            id: listColumn
            visible: !root.reading && root.view === "inbox"
            width: parent.width
            spacing: Style.space(4)

            Repeater {
              model: root.shown
              MessageRow {
                required property var modelData
                required property int index
                width: listColumn.width
                message: modelData
                rowIndex: index
              }
            }

            Text {
              visible: root.hole !== "" && root.holeNote !== ""
              width: parent.width
              leftPadding: Style.space(10)
              rightPadding: Style.space(10)
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: root.holeNote
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            Text {
              visible: (root.hole === "" ? root.watched : (!holeReader.running && root.holeNote === "")) && root.shown.length === 0
              width: parent.width
              leftPadding: Style.space(10)
              textFormat: Text.PlainText
              text: "Nothing waiting."
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
            }
          }

          // ---- the reader ----

          Column {
            visible: root.reading !== null
            width: parent.width
            spacing: Style.space(10)

            RowLayout {
              width: parent.width
              spacing: Style.space(6)

              PanelActionButton {
                iconText: "󰁍"
                tooltipText: "Back (b)"
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: root.closeMessage()
              }

              Text {
                Layout.fillWidth: true
                textFormat: Text.PlainText
                text: root.reading ? (root.reading.tags || []).join(" · ") : ""
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                elide: Text.ElideRight
              }

              PanelActionButton {
                iconText: "󰅺"
                tooltipText: "The whole thread, in the browser (t)"
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: if (root.reading) root.runAction(["open", "--thread", String(root.reading.id)].concat(root.forArgs()), "Thread")
              }

              PanelActionButton {
                visible: !!root.reading && root.reading.from !== "automation"
                iconText: "󰭹"
                tooltipText: "Everything with them, in the browser (c)"
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: if (root.reading) root.runAction(["open", "--with", String(root.reading.from)].concat(root.forArgs()), "Conversation")
              }

              PanelActionButton {
                iconText: "󰇮"
                tooltipText: "Mark unread (u)"
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: if (root.reading) root.putBackMessage(root.reading)
              }

              PanelActionButton {
                iconText: "󰀼"
                tooltipText: "Archive (a)"
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: if (root.reading) root.archiveMessage(root.reading)
              }
            }

            Text {
              visible: !!root.reading && root.reading.signature !== "verified"
              width: parent.width
              leftPadding: Style.space(10)
              rightPadding: Style.space(10)
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: root.reading ? String(root.reading.signature_note || "") : ""
              color: root.reading && root.reading.signature === "bad" ? root.urgentColor : root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            Text {
              visible: root.readingNote !== ""
              width: parent.width
              leftPadding: Style.space(10)
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: root.readingNote
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            // The CLI's HTML: escaped, no images. Links go to the browser.
            Text {
              width: parent.width
              leftPadding: Style.space(10)
              rightPadding: Style.space(10)
              textFormat: Text.RichText
              wrapMode: Text.Wrap
              text: root.readingHtml
              color: root.foreground
              linkColor: root.unreadColor
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              onLinkActivated: function(link) { root.openLink(link) }
              MouseArea {
                anchors.fill: parent
                acceptedButtons: Qt.NoButton
                cursorShape: parent.hoveredLink ? Qt.PointingHandCursor : Qt.ArrowCursor
              }
            }

            Text {
              visible: !!root.reading && (root.reading.attachments || []).length > 0
              width: parent.width
              leftPadding: Style.space(10)
              rightPadding: Style.space(10)
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: root.reading ? "Attached: " + (root.reading.attachments || []).join(", ")
                + ". Fetch one with deaddrop file " + root.reading.id + " NAME" : ""
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
          }
        }
      }
    }
  }

  component Chip: Rectangle {
    id: chip
    property string label: ""
    property bool on: false
    signal picked()
    radius: height / 2
    implicitHeight: chipText.implicitHeight + Style.space(6)
    implicitWidth: Math.min(chipText.implicitWidth, Style.space(180)) + Style.space(16)
    color: on ? Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.14) : "transparent"
    border.width: 1
    border.color: on ? root.foreground : root.dim

    Text {
      id: chipText
      anchors.centerIn: parent
      width: Math.min(implicitWidth, Style.space(180))
      elide: Text.ElideRight
      textFormat: Text.PlainText
      text: chip.label
      color: chip.on ? root.foreground : root.dim
      font.family: root.fontFamily
      font.pixelSize: Style.font.caption
    }

    MouseArea {
      anchors.fill: parent
      cursorShape: Qt.PointingHandCursor
      onClicked: chip.picked()
    }
  }

  // A thread: its subject, who's in it, how many letters, how long ago.
  // Opens the whole thread in the browser.
  component TopicRow: CursorSurface {
    id: trow
    property var topic: ({})
    property int rowIndex: 0
    readonly property bool hasUnread: (topic.unread || 0) > 0

    hasCursor: root.cursorActive && root.view === "threads" && root.cursor === rowIndex
    foreground: root.foreground
    implicitHeight: tcontent.implicitHeight + Style.spacing.rowPaddingX

    MouseArea {
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onEntered: { root.cursorActive = true; root.cursor = trow.rowIndex }
      onClicked: root.openTopic(trow.topic)
    }

    RowLayout {
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      anchors.leftMargin: Style.space(10)
      anchors.rightMargin: Style.space(10)
      spacing: Style.space(8)

      Rectangle {
        Layout.alignment: Qt.AlignTop
        Layout.topMargin: Style.space(6)
        implicitWidth: Style.space(8)
        implicitHeight: Style.space(8)
        radius: width / 2
        color: trow.hasUnread ? root.unreadColor : "transparent"
        border.width: trow.hasUnread ? 0 : 1
        border.color: root.dim
      }

      ColumnLayout {
        id: tcontent
        Layout.fillWidth: true
        spacing: Style.space(1)

        Text {
          Layout.fillWidth: true
          textFormat: Text.PlainText
          text: String(trow.topic.subject || "")
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          font.bold: trow.hasUnread
          elide: Text.ElideRight
        }

        Text {
          Layout.fillWidth: true
          textFormat: Text.PlainText
          text: (trow.topic.speakers || []).join(", ") + " · " + trow.topic.count + (trow.topic.count === 1 ? " letter" : " letters")
            + (trow.hasUnread ? " · " + trow.topic.unread + " new" : "") + " · " + root.ago(trow.topic.last)
          color: root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          elide: Text.ElideRight
        }
      }
    }
  }

  component MessageRow: CursorSurface {
    id: row
    property var message: ({})
    property int rowIndex: 0
    readonly property bool isUnread: message.state === "unread"
    readonly property bool isUrgent: isUnread && message.urgency === "urgent"

    hasCursor: root.cursorActive && root.view === "inbox" && root.cursor === rowIndex
    foreground: root.foreground
    implicitHeight: content.implicitHeight + Style.spacing.rowPaddingX

    MouseArea {
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onEntered: { root.cursorActive = true; root.cursor = row.rowIndex }
      onClicked: root.openMessage(row.message)
    }

    RowLayout {
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      anchors.leftMargin: Style.space(10)
      anchors.rightMargin: Style.space(6)
      spacing: Style.space(8)

      Rectangle {
        Layout.alignment: Qt.AlignTop
        Layout.topMargin: Style.space(6)
        implicitWidth: Style.space(8)
        implicitHeight: Style.space(8)
        radius: width / 2
        color: row.isUrgent ? root.urgentColor : (row.isUnread ? root.unreadColor : "transparent")
        border.width: row.isUnread ? 0 : 1
        border.color: root.dim
      }

      ColumnLayout {
        id: content
        Layout.fillWidth: true
        spacing: Style.space(1)

        Text {
          Layout.fillWidth: true
          textFormat: Text.PlainText
          text: String(row.message.subject || "")
          color: row.isUrgent ? root.urgentColor : root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          font.bold: row.isUnread
          elide: Text.ElideRight
        }

        Text {
          Layout.fillWidth: true
          textFormat: Text.PlainText
          text: root.sender(row.message) + " · " + root.ago(row.message.sent)
            + ((row.message.tags || []).length ? " · " + row.message.tags.join(", ") : "")
            + ((row.message.attachments || []).length ? " · " + row.message.attachments.length + " attached" : "")
          color: row.message.signature === "bad" ? root.urgentColor : root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          elide: Text.ElideRight
        }
      }

      PanelActionButton {
        Layout.alignment: Qt.AlignVCenter
        iconText: "󰀼"
        tooltipText: "Archive"
        foreground: root.foreground
        fontFamily: root.fontFamily
        onClicked: root.archiveMessage(row.message)
      }
    }
  }
}
