#!/bin/sh
# Fly mounts a volume as root:root 0755. The upstream keeper image runs the
# daemon as the `clickhouse` user and does not chown the data directory in
# its entrypoint, so the daemon cannot create coordination/ inside the mount
# and crash-loops on "Permission denied". Chown first, then hand off to the
# upstream entrypoint, which drops privileges. Idempotent: chown -R on an
# already-owned tree is fast.
set -e
chown -R clickhouse:clickhouse /var/lib/clickhouse-keeper
exec /entrypoint.sh "$@"
