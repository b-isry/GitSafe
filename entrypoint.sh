#!/bin/sh
set -e

DATA_DIR="${GITSAFE_DATA_DIR:-/var/data}"
mkdir -p "$DATA_DIR"
chown -R gitsafe:gitsafe "$DATA_DIR" || true

exec su-exec gitsafe:gitsafe "$@"