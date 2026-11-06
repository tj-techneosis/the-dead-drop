#!/bin/sh
# A SwiftBar (or xbar) plugin: your Dead Drop in the macOS menu bar.
# Copy it into SwiftBar's plugin folder; the 1m in the name is how often it
# re-reads the inbox file (which costs nothing: no network). Something else
# keeps that file current: `deaddrop watch` on a timer (examples/watch/).
#
# The icon is an envelope with the unread count, red when some is urgent.
# The menu lists your mail, unread first; each message has Read (opens it in
# the browser and marks it read), Mark read or unread, and Archive.
#
# <xbar.title>Dead Drop</xbar.title>
# <xbar.desc>Your Dead Drop inbox.</xbar.desc>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
# <swiftbar.hideLastUpdated>true</swiftbar.hideLastUpdated>
exec "$HOME/.local/bin/deaddrop" bar --format swiftbar
