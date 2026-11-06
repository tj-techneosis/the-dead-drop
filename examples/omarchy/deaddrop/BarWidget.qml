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
    if (root.cursor > root.messages.length) root.cursor = root.messages.length
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

  function runAction(cmd, note) {
    if (actor.running) return
    root.actionNote = ""
    actor.label = note
    actor.command = [root.deaddrop].concat(cmd)
    actor.running = true
  }

  function checkNow() { root.runAction(["watch", "--no-hooks"], "Check") }

  function openMessage(m) {
    root.reading = m
    root.readingHtml = ""
    root.readingNote = "Opening…"
    reader.command = [root.deaddrop, "show", String(m.id), "--html", "--fragment"]
    reader.running = true
  }

  function closeMessage() {
    root.reading = null
    root.readingHtml = ""
    root.readingNote = ""
  }

  function archiveMessage(m) { root.runAction(["mark", "archive", String(m.id)], "Archive"); root.closeMessage() }
  function putBackMessage(m) { root.runAction(["mark", "unread", String(m.id)], "Mark unread"); root.closeMessage() }
  function markMessageRead(m) { root.runAction(["mark", "read", String(m.id)], "Mark read") }

  function openLink(link) {
    var l = String(link).toLowerCase()
    if (l.indexOf("https://") === 0 || l.indexOf("http://") === 0 || l.indexOf("mailto:") === 0)
      Qt.openUrlExternally(link)
  }

  function moveCursor(dy) {
    root.cursor = Math.max(0, Math.min(root.messages.length - 1, root.cursor + dy))
  }

  onOpenedChanged: if (opened) {
    cursorActive = false
    cursor = 0
    closeMessage()
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
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (root.reading) root.readingHtml = text
    }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (text.trim() !== "") root.readingNote = text.trim().replace(/^deaddrop: /, "")
    }
    onExited: function(exitCode) {
      if (exitCode !== 0 || !root.reading) return
      root.readingNote = ""
      if (root.reading.state === "unread") root.markMessageRead(root.reading)
    }
  }

  Process {
    id: actor
    property string label: ""
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: if (text.trim() !== "") root.actionNote = actor.label + " didn't work: " + text.trim().replace(/^deaddrop: /, "")
    }
    // mark and watch rewrite the inbox file; the FileView picks that up.
    onExited: function(exitCode) { inboxFile.reload() }
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
      onActivateRequested: if (!root.reading && root.cursorActive && root.messages[root.cursor]) root.openMessage(root.messages[root.cursor])
      onCloseRequested: if (root.reading) root.closeMessage(); else root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(t) {
        var k = String(t).toLowerCase()
        var m = root.reading || (root.cursorActive ? root.messages[root.cursor] : null)
        if (k === "r") root.checkNow()
        else if (k === "a" && m) root.archiveMessage(m)
        else if (k === "u" && m) root.putBackMessage(m)
        else if (k === "b" && root.reading) root.closeMessage()
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
              ? "From " + root.reading.from + " · " + root.ago(root.reading.sent) + (root.reading.urgency === "urgent" ? " · urgent" : "")
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

          // ---- the list ----

          Column {
            id: listColumn
            visible: !root.reading
            width: parent.width
            spacing: Style.space(4)

            Repeater {
              model: root.messages
              MessageRow {
                required property var modelData
                required property int index
                width: listColumn.width
                message: modelData
                rowIndex: index
              }
            }

            Text {
              visible: root.watched && root.messages.length === 0
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

  component MessageRow: CursorSurface {
    id: row
    property var message: ({})
    property int rowIndex: 0
    readonly property bool isUnread: message.state === "unread"
    readonly property bool isUrgent: isUnread && message.urgency === "urgent"

    hasCursor: root.cursorActive && root.cursor === rowIndex
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
          text: row.message.from + " · " + root.ago(row.message.sent)
            + ((row.message.tags || []).length ? " · " + row.message.tags.join(", ") : "")
            + ((row.message.attachments || []).length ? " · " + row.message.attachments.length + " attached" : "")
          color: root.dim
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
