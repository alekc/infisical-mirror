#!/bin/sh
# Purge drops the state file, per Debian convention. That costs something: the
# next run then has no record of what it reconciled, so every key looks new on
# both sides. A plain `remove` keeps it, and reinstalling resumes cleanly.
set -e

if [ "$1" = "purge" ]; then
	rm -rf /var/lib/infisical-mirror
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi
