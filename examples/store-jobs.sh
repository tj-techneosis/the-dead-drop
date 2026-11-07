#!/bin/sh
# The jobs a store's machine runs, booked once with Crontinental. They live
# with the machine's own setup, not in this repo.

# Keep the board current: one lookout per box, mail/<who>/inbox. The board
# is the notification, so the job itself only notifies when it fails;
# `deaddrop firetower` takes the next due time from $CRONTINENTAL_NEXT, so a
# stopped job shows on the board as "no word".
crontinental every 5m --name deaddrop-firetower --forever --notify fail -- \
  deaddrop firetower

# Or: only the boxes people are meant to empty (not shared shelves), by how
# long mail has waited unheard, and ring whoever minds a box once when it
# goes to fire (route deaddrop-box=<box> in this machine's notify hooks):
#   deaddrop firetower --box alice --box alice-helper --box bob \
#     --smoke-after 30m --fire-after 3h --ring

# Archived mail goes after 30 days.
crontinental every 04:00 --name deaddrop-gc --forever --notify fail -- \
  deaddrop gc
