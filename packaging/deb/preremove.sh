#!/bin/sh
# dpkg passes "remove" for a real removal and "upgrade <version>" mid-upgrade.
# Only the first should stop the service: stopping on upgrade would leave a
# deployment down whenever the postinstall try-restart found nothing running.
set -e

if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
	if [ -d /run/systemd/system ]; then
		systemctl --no-reload disable --now infisical-mirror.service >/dev/null 2>&1 || true
	fi
fi
