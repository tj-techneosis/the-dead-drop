#!/bin/sh
# The jobs a store's machine runs, booked once with Crontinental. They live
# with the machine's own setup, not in this repo.

# Keep the board current: one lookout per box, mail/<who>/inbox. The board
# is the notification, so --notify never; `deaddrop firetower` takes the next
# due time from $CRONTINENTAL_NEXT.
crontinental every 5m --name deaddrop-firetower --forever --notify never -- \
  deaddrop firetower

# Archived mail goes after 30 days.
crontinental every 04:00 --name deaddrop-gc --forever --notify fail -- \
  deaddrop gc
