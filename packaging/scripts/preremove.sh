#!/bin/sh
# mcwsd preremove for .deb / .rpm: stop and disable the service so that the
# binary can be replaced or removed without leaving a crash-looping unit.
set -e

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl stop mcwsd.service >/dev/null 2>&1 || true
    systemctl disable mcwsd.service >/dev/null 2>&1 || true
fi

exit 0
