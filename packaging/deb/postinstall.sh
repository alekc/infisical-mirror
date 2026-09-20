#!/bin/sh
# Deliberately does not enable or start the service: the shipped config has
# placeholder slugs and no credentials, so it would only crashloop, and a
# package that starts writing to secret stores on install is a bad default.
set -e

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true

	# On upgrade, pick up the new unit for an already-running deployment. A
	# first install has nothing active, so try-restart is a no-op there.
	if [ "$1" = "configure" ] && [ -n "$2" ]; then
		systemctl try-restart infisical-mirror.service >/dev/null 2>&1 || true
	fi
fi
